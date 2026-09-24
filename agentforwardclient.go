package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// RunAgentForward is the client entry point for `deskconn shell -A`. It
// opens one session-level raw stream/channel, tells the device to start
// forwarding, signals ready once the device acks, and for every external
// connection the device reports, dials agentSock  and relays.
func RunAgentForward(ctx context.Context, mode, realm, cfgDirectory, agentSock string, ready chan<- error) error {
	authID, _, err := clientCredentials(realm, cfgDirectory)
	if err != nil {
		ready <- err
		return err
	}

	switch mode {
	case modeP2P:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			ready <- err
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return runAgentForwardP2P(ctx, p2pSess, authID, agentSock, ready)
	case modeQUIC:
		return runAgentForwardQUIC(ctx, realm, cfgDirectory, authID, agentSock, ready)
	default:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return runAgentForwardP2P(ctx, p2pSess, authID, agentSock, ready)
		}
		fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
		return runAgentForwardQUIC(ctx, realm, cfgDirectory, authID, agentSock, ready)
	}
}

func runAgentForwardQUIC(ctx context.Context, realm, cfgDirectory, authID, agentSock string, ready chan<- error) error {
	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		ready <- err
		return err
	}
	defer func() { _ = quicSess.Connection().Close() }()

	stream, err := quicSess.OpenStream()
	if err != nil {
		ready <- err
		return err
	}
	defer stream.Close()
	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpAgentForward}); err != nil {
		ready <- err
		return err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		ready <- err
		return err
	}
	conn := &quicClientShellConn{stream: stream}
	if err := startAgentForward(conn, sendKey, receiveKey, authID); err != nil {
		ready <- err
		return err
	}
	ready <- nil

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() { doneOnce.Do(func() { close(done); _ = stream.Close() }) }
	defer closeAll()
	SafeGo(func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	})

	writer := &portReverseWriter{ch: make(chan []byte, 64), done: done}
	SafeGo(func() {
		for {
			select {
			case env := <-writer.ch:
				if err := WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			case <-done:
				return
			}
		}
	})

	readNext := func() (byte, []byte, error) {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := ReadFrame(stream)
		if err != nil {
			return 0, nil, err
		}
		return DecryptEnvelope(frame, receiveKey)
	}
	return runAgentForwardClientLoop(ctx, readNext, writer, sendKey, agentSock)
}

func runAgentForwardP2P(ctx context.Context, p2pSess P2PChannelOpener,
	authID, agentSock string, ready chan<- error) error {
	channel, err := openP2PChannel(p2pSess, AgentForwardChannelLabel)
	if err != nil {
		ready <- err
		return err
	}
	defer channel.Close()
	closed, _ := WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		ready <- err
		return err
	}
	conn := newP2PClientShellConn(channel, closed)
	if err := startAgentForward(conn, sendKey, receiveKey, authID); err != nil {
		ready <- err
		return err
	}
	ready <- nil

	writer := &portReverseWriter{ch: make(chan []byte, 64), done: closed}
	SafeGo(func() {
		for {
			select {
			case env := <-writer.ch:
				if err := channel.Send(env); err != nil {
					_ = channel.Close()
					return
				}
			case <-closed:
				return
			}
		}
	})
	SafeGo(func() {
		select {
		case <-ctx.Done():
			_ = channel.Close()
		case <-closed:
		}
	})

	readNext := func() (byte, []byte, error) {
		frame, err := RecvPriority(conn.msgCh, closed, shellIdleTimeout)
		if err != nil {
			return 0, nil, err
		}
		return DecryptEnvelope(frame, receiveKey)
	}
	return runAgentForwardClientLoop(ctx, readNext, writer, sendKey, agentSock)
}

// startAgentForward sends the initial "start forwarding" request and waits
// for the device's ack, returning an error if it can't proceed.
func startAgentForward(conn shellConn, sendKey, receiveKey []byte, authID string) error {
	env, err := buildPortEnvelope(agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: authID}), sendKey)
	if err != nil {
		return err
	}
	if err := conn.sendEnvelope(env); err != nil {
		return err
	}
	frame, err := conn.recvEnvelope()
	if err != nil {
		return err
	}
	_, plaintext, err := DecryptEnvelope(frame, receiveKey)
	if err != nil {
		return err
	}
	var ack agentForwardMsg
	if err := json.Unmarshal(plaintext, &ack); err != nil {
		return err
	}
	if ack.Error != "" {
		return errors.New(ack.Error)
	}
	return nil
}

// runAgentForwardClientLoop is the transport-agnostic heart of both
// runAgentForwardQUIC and runAgentForwardP2P: readNext blocks for the next
// decrypted envelope, writer is this session's single wire-write owner, and
// sendKey encrypts what this loop sends back.
func runAgentForwardClientLoop(ctx context.Context, readNext func() (byte, []byte, error), writer *portReverseWriter,
	sendKey []byte, agentSock string) error {
	var mu sync.Mutex
	localConns := make(map[uint64]net.Conn)
	defer func() {
		mu.Lock()
		for _, c := range localConns {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	for {
		kind, plaintext, err := readNext()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		switch kind {
		case agentFwdMsgControl:
			var msg agentForwardMsg
			if json.Unmarshal(plaintext, &msg) != nil {
				continue
			}
			switch msg.Op {
			case agentFwdOpConnect:
				connID := msg.ConnID
				localConn, dialErr := net.Dial("unix", agentSock)
				if dialErr != nil {
					if env, encErr := buildPortEnvelope(agentFwdMsgControl,
						mustJSON(agentForwardMsg{Op: agentFwdOpClose, ConnID: connID}), sendKey); encErr == nil {
						writer.send(env)
					}
					continue
				}
				mu.Lock()
				localConns[connID] = localConn
				mu.Unlock()
				SafeGo(func() { pumpAgentForwardLocalConn(localConn, connID, sendKey, writer, &mu, localConns) })
			case agentFwdOpClose:
				mu.Lock()
				c, ok := localConns[msg.ConnID]
				delete(localConns, msg.ConnID)
				mu.Unlock()
				if ok {
					_ = c.Close()
				}
			}
		case agentFwdMsgData:
			connID, payload, ok := decodeConnData(plaintext)
			if !ok {
				continue
			}
			mu.Lock()
			c, ok := localConns[connID]
			mu.Unlock()
			if ok {
				_, _ = c.Write(payload)
			}
		}
	}
}

// pumpAgentForwardLocalConn reads localConn (the local ssh-agent socket
// connection this client just dialed on the device's behalf) and forwards
// its bytes back over writer, tagged with connID, until it errors or
// closes.
func pumpAgentForwardLocalConn(localConn net.Conn, connID uint64, sendKey []byte, writer *portReverseWriter,
	mu *sync.Mutex, localConns map[uint64]net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := localConn.Read(buf)
		if n > 0 {
			env, encErr := buildPortEnvelope(agentFwdMsgData, encodeConnData(connID, buf[:n]), sendKey)
			if encErr != nil || !writer.send(env) {
				break
			}
		}
		if readErr != nil {
			break
		}
	}
	mu.Lock()
	_, stillActive := localConns[connID]
	delete(localConns, connID)
	mu.Unlock()
	_ = localConn.Close()
	if stillActive {
		if env, encErr := buildPortEnvelope(agentFwdMsgControl,
			mustJSON(agentForwardMsg{Op: agentFwdOpClose, ConnID: connID}), sendKey); encErr == nil {
			writer.send(env)
		}
	}
}
