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

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

// RunPortForward is the client entry point for `deskconn port forward`. It
// listens on localPort and, for each accepted local connection, opens a
// fresh raw stream/channel and asks the device to dial remotePort, then
// relays. mode picks "quic"/"p2p" directly, or "" tries P2P first and
// falls back to QUIC on error, matching file transfer's default-mode policy.
func RunPortForward(ctx context.Context, mode, realm, cfgDirectory, remotePort, localPort string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:"+localPort)
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%s: %w", localPort, err)
	}
	defer ln.Close()

	switch mode {
	case modeP2P:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return acceptPortForwardLoopP2P(ctx, ln, p2pSess, remotePort)
	case modeQUIC:
		quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = quicSess.Connection().Close() }()
		return acceptPortForwardLoopQUIC(ctx, ln, quicSess, realm, remotePort)
	default:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return acceptPortForwardLoopP2P(ctx, ln, p2pSess, remotePort)
		}
		fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
		quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = quicSess.Connection().Close() }()
		return acceptPortForwardLoopQUIC(ctx, ln, quicSess, realm, remotePort)
	}
}

func acceptPortForwardLoopQUIC(ctx context.Context, ln net.Listener, quicSess *xconn.QUICSession,
	realm, remotePort string) error {
	SafeGo(func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = quicSess.Connection().Close()
	})
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		SafeGo(func() { forwardOneConnectionQUIC(quicSess, realm, conn, remotePort) })
	}
}

func forwardOneConnectionQUIC(quicSess *xconn.QUICSession, realm string, localConn net.Conn, remotePort string) {
	stream, err := quicSess.OpenStream()
	if err != nil {
		_ = localConn.Close()
		return
	}
	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpPortForward}); err != nil {
		_ = stream.Close()
		_ = localConn.Close()
		return
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		_ = stream.Close()
		_ = localConn.Close()
		return
	}
	conn := &quicClientShellConn{stream: stream}
	if !completePortForwardHandshake(conn, sendKey, receiveKey, remotePort) {
		_ = stream.Close()
		_ = localConn.Close()
		return
	}
	relayPortForwardQUIC(stream, localConn, sendKey, receiveKey)
}

func acceptPortForwardLoopP2P(ctx context.Context, ln net.Listener, p2pSess P2PChannelOpener, remotePort string) error {
	closer, ok := p2pSess.(interface{ Close() error })
	SafeGo(func() {
		<-ctx.Done()
		_ = ln.Close()
		if ok {
			_ = closer.Close()
		}
	})
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		SafeGo(func() { forwardOneConnectionP2P(p2pSess, conn, remotePort) })
	}
}

func forwardOneConnectionP2P(p2pSess P2PChannelOpener, localConn net.Conn, remotePort string) {
	channel, err := openP2PChannel(p2pSess, PortForwardChannelLabel)
	if err != nil {
		_ = localConn.Close()
		return
	}
	closed, _ := WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		_ = channel.Close()
		_ = localConn.Close()
		return
	}
	conn := newP2PClientShellConn(channel, closed)
	if !completePortForwardHandshake(conn, sendKey, receiveKey, remotePort) {
		_ = channel.Close()
		_ = localConn.Close()
		return
	}
	relayPortForwardP2P(channel, localConn, sendKey, receiveKey, conn.msgCh, closed)
}

// completePortForwardHandshake sends the "dial this port" request over conn
// (either transport, via shellConn from shellclient.go) and waits for the
// device's ack, returning false (caller should give up) on any failure,
// including the device reporting a dial error.
func completePortForwardHandshake(conn shellConn, sendKey, receiveKey []byte, remotePort string) bool {
	env, err := buildPortEnvelope(portMsgControl, mustJSON(portForwardControlMsg{Port: remotePort}), sendKey)
	if err != nil {
		return false
	}
	if err := conn.sendEnvelope(env); err != nil {
		return false
	}
	frame, err := conn.recvEnvelope()
	if err != nil {
		return false
	}
	_, plaintext, err := DecryptEnvelope(frame, receiveKey)
	if err != nil {
		return false
	}
	var ack portForwardControlMsg
	if err := json.Unmarshal(plaintext, &ack); err != nil {
		return false
	}
	if ack.Error != "" {
		log.Debugf("port forward: device could not dial port %s: %s", remotePort, ack.Error)
		return false
	}
	return true
}

// RunPortReverse is the client entry point for `deskconn port reverse`. It
// opens one session-level raw stream/channel, asks the device to listen on
// remotePort, and for every external connection the device reports, dials
// localhost:localPort and relays. Same mode/fallback policy as
// RunPortForward.
func RunPortReverse(ctx context.Context, mode, realm, cfgDirectory, remotePort, localPort string) error {
	switch mode {
	case modeP2P:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return runPortReverseP2P(ctx, p2pSess, remotePort, localPort)
	case modeQUIC:
		return runPortReverseQUIC(ctx, realm, cfgDirectory, remotePort, localPort)
	default:
		p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return runPortReverseP2P(ctx, p2pSess, remotePort, localPort)
		}
		fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
		return runPortReverseQUIC(ctx, realm, cfgDirectory, remotePort, localPort)
	}
}

func runPortReverseQUIC(ctx context.Context, realm, cfgDirectory, remotePort, localPort string) error {
	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = quicSess.Connection().Close() }()

	stream, err := quicSess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpPortReverse}); err != nil {
		return err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return err
	}
	conn := &quicClientShellConn{stream: stream}
	if err := startPortReverseListen(conn, sendKey, receiveKey, remotePort); err != nil {
		return err
	}

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
	return runPortReverseClientLoop(ctx, readNext, writer, sendKey, localPort)
}

func runPortReverseP2P(ctx context.Context, p2pSess P2PChannelOpener, remotePort, localPort string) error {
	channel, err := openP2PChannel(p2pSess, PortReverseChannelLabel)
	if err != nil {
		return err
	}
	defer channel.Close()
	closed, _ := WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		return err
	}
	conn := newP2PClientShellConn(channel, closed)
	if err := startPortReverseListen(conn, sendKey, receiveKey, remotePort); err != nil {
		return err
	}

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
	return runPortReverseClientLoop(ctx, readNext, writer, sendKey, localPort)
}

// startPortReverseListen sends the initial "listen on remotePort" request
// and waits for the device's ack, returning an error (including the
// device's own reported listen failure) if it can't proceed.
func startPortReverseListen(conn shellConn, sendKey, receiveKey []byte, remotePort string) error {
	env, err := buildPortEnvelope(portMsgControl,
		mustJSON(portReverseMsg{Op: portRevOpListen, RemotePort: remotePort}), sendKey)
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
	var ack portReverseMsg
	if err := json.Unmarshal(plaintext, &ack); err != nil {
		return err
	}
	if ack.Error != "" {
		return errors.New(ack.Error)
	}
	return nil
}

// runPortReverseClientLoop is the transport-agnostic heart of both
// runPortReverseQUIC and runPortReverseP2P: readNext blocks for the next
// decrypted envelope, writer is this session's single wire-write owner (see
// portReverseWriter), and sendKey encrypts what this loop sends back.
// Mirrors handleQUICPortReverseStream/servePortReverseChannel's device-side
// loop.
func runPortReverseClientLoop(ctx context.Context, readNext func() (byte, []byte, error), writer *portReverseWriter,
	sendKey []byte, localPort string) error {
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
		case portMsgControl:
			var msg portReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil {
				continue
			}
			switch msg.Op {
			case portRevOpConnect:
				connID := msg.ConnID
				localConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", localPort))
				if dialErr != nil {
					if env, encErr := buildPortEnvelope(portMsgControl,
						mustJSON(portReverseMsg{Op: portRevOpClose, ConnID: connID}), sendKey); encErr == nil {
						writer.send(env)
					}
					continue
				}
				mu.Lock()
				localConns[connID] = localConn
				mu.Unlock()
				SafeGo(func() { pumpPortReverseLocalConn(localConn, connID, sendKey, writer, &mu, localConns) })
			case portRevOpClose:
				mu.Lock()
				c, ok := localConns[msg.ConnID]
				delete(localConns, msg.ConnID)
				mu.Unlock()
				if ok {
					_ = c.Close()
				}
			}
		case portMsgData:
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

// pumpPortReverseLocalConn reads localConn (the connection this client just
// dialed to localhost:localPort on the device's behalf) and forwards its
// bytes back over writer, tagged with connID, until it errors or closes.
func pumpPortReverseLocalConn(localConn net.Conn, connID uint64, sendKey []byte, writer *portReverseWriter,
	mu *sync.Mutex, localConns map[uint64]net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := localConn.Read(buf)
		if n > 0 {
			env, encErr := buildPortEnvelope(portMsgData, encodeConnData(connID, buf[:n]), sendKey)
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
		if env, encErr := buildPortEnvelope(portMsgControl,
			mustJSON(portReverseMsg{Op: portRevOpClose, ConnID: connID}), sendKey); encErr == nil {
			writer.send(env)
		}
	}
}
