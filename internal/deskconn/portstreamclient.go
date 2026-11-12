package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
)

// RunPortForward is the client entry point for `desk port forward`. It
// listens on localPort and, for each accepted local connection, opens a
// fresh RPC-associated duplex stream and asks the machine to dial remotePort, then
// relays. mode picks "routed"/"direct" itself, or "" uses userd's persistent
// connection, falling back (without userd) to trying P2P first, then QUIC
// -- matching file transfer's default-mode policy.
func RunPortForward(ctx context.Context, mode, realm, cfgDirectory, remotePort, localPort string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:"+localPort)
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%s: %w", localPort, err)
	}
	defer ln.Close()

	switch mode {
	case ModeDirect:
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return acceptPortForwardLoopP2P(ctx, ln, p2pSess, remotePort)
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = quicSess.Connection().Close() }()
		return acceptPortForwardLoopQUIC(ctx, ln, quicSess, remotePort)
	default:
		daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = daemon.Close() }()
			if daemon.P2P() {
				return acceptPortForwardLoopP2P(ctx, ln, daemon, remotePort)
			}
			return acceptPortForwardLoopQUIC(ctx, ln, daemon, remotePort)
		}
		if !errors.Is(err, ErrDaemonUnavailable) {
			return err
		}
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return acceptPortForwardLoopP2P(ctx, ln, p2pSess, remotePort)
		}
		fmt.Fprintln(os.Stderr, "direct connection unavailable, falling back to routed")
		quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = quicSess.Connection().Close() }()
		return acceptPortForwardLoopQUIC(ctx, ln, quicSess, remotePort)
	}
}

func acceptPortForwardLoopQUIC(ctx context.Context, ln net.Listener, quicSess common.StreamSession,
	remotePort string) error {
	closer, ok := quicSess.(interface{ Connection() io.Closer })
	return acceptPortForwardConnections(ctx, ln, func() {
		if ok {
			_ = closer.Connection().Close()
		}
	}, func(conn net.Conn) {
		forwardOneConnectionQUIC(ctx, quicSess, conn, remotePort)
	})
}

func acceptPortForwardConnections(ctx context.Context, ln net.Listener,
	closeSession func(), forward func(net.Conn)) error {
	common.SafeGo(func() {
		<-ctx.Done()
		_ = ln.Close()
		closeSession()
	})
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		common.SafeGo(func() { forward(conn) })
	}
}

func forwardOneConnectionQUIC(ctx context.Context, quicSess common.StreamSession,
	localConn net.Conn, remotePort string) {
	stream, err := common.OpenRPCStream(ctx, quicSess, common.ProcedurePortForwardStream, false)
	if err != nil {
		_ = localConn.Close()
		return
	}

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		_ = stream.Close()
		_ = localConn.Close()
		return
	}
	conn := &streamClientShellConn{stream: stream}
	if !completePortForwardHandshake(conn, sendKey, receiveKey, remotePort) {
		_ = stream.Close()
		_ = localConn.Close()
		return
	}
	common.RelayPortForwardQUIC(stream, localConn, sendKey, receiveKey)
}

func acceptPortForwardLoopP2P(ctx context.Context, ln net.Listener,
	p2pSess common.StreamSession, remotePort string) error {
	closer, ok := p2pSess.(interface{ Close() error })
	return acceptPortForwardConnections(ctx, ln, func() {
		if ok {
			_ = closer.Close()
		}
	}, func(conn net.Conn) {
		forwardOneConnectionQUIC(ctx, p2pSess, conn, remotePort)
	})
}

// completePortForwardHandshake sends the "dial this port" request over conn
// (either transport, via shellConn from shellclient.go) and waits for the
// machine's ack, returning false (caller should give up) on any failure,
// including the machine reporting a dial error.
func completePortForwardHandshake(conn shellConn, sendKey, receiveKey []byte, remotePort string) bool {
	env, err := common.BuildPortEnvelope(common.PortMsgControl,
		common.MustJSON(common.PortForwardControlMsg{Port: remotePort}), sendKey)
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
	_, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
	if err != nil {
		return false
	}
	var ack common.PortForwardControlMsg
	if err := json.Unmarshal(plaintext, &ack); err != nil {
		return false
	}
	if ack.Error != "" {
		log.Debugf("port forward: machine could not dial port %s: %s", remotePort, ack.Error)
		return false
	}
	return true
}

// RunPortReverse is the client entry point for `desk port reverse`. It
// opens one RPC-associated duplex stream, asks the machine to listen on
// remotePort, and for every external connection the machine reports, dials
// localhost:localPort and relays. Same mode/fallback policy as
// RunPortForward.
func RunPortReverse(ctx context.Context, mode, realm, cfgDirectory, remotePort, localPort string) error {
	switch mode {
	case ModeDirect:
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Close() }()
		return runPortReverseOnStream(ctx, p2pSess, remotePort, localPort)
	case ModeRouted:
		return runPortReverseQUIC(ctx, realm, cfgDirectory, remotePort, localPort)
	default:
		daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = daemon.Close() }()
			return runPortReverseOnStream(ctx, daemon, remotePort, localPort)
		}
		if !errors.Is(err, ErrDaemonUnavailable) {
			return err
		}
		p2pSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err == nil {
			defer func() { _ = p2pSess.Close() }()
			return runPortReverseOnStream(ctx, p2pSess, remotePort, localPort)
		}
		fmt.Fprintln(os.Stderr, "direct connection unavailable, falling back to routed")
		return runPortReverseQUIC(ctx, realm, cfgDirectory, remotePort, localPort)
	}
}

func runPortReverseQUIC(ctx context.Context, realm, cfgDirectory, remotePort, localPort string) error {
	quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = quicSess.Connection().Close() }()
	return runPortReverseOnStream(ctx, quicSess, remotePort, localPort)
}

// runPortReverseOnStream runs the reverse-forward session on an RPC stream returned by sess.
func runPortReverseOnStream(ctx context.Context, sess common.StreamSession,
	remotePort, localPort string) error {
	stream, err := common.OpenRPCStream(ctx, sess, common.ProcedurePortReverseStream, false)
	if err != nil {
		return err
	}
	defer stream.Close()

	sendKey, receiveKey, err := StreamClientKeyExchange(stream)
	if err != nil {
		return err
	}
	conn := &streamClientShellConn{stream: stream}
	if err := startPortReverseListen(conn, sendKey, receiveKey, remotePort); err != nil {
		return err
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() { doneOnce.Do(func() { close(done); _ = stream.Close() }) }
	defer closeAll()
	common.SafeGo(func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	})

	writer := &common.PortReverseWriter{Ch: make(chan []byte, 64), Done: done}
	common.SafeGo(func() {
		for {
			select {
			case env := <-writer.Ch:
				if err := common.WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			case <-done:
				return
			}
		}
	})

	readNext := func() (byte, []byte, error) {
		_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
		frame, err := common.ReadFrame(stream)
		if err != nil {
			return 0, nil, err
		}
		return common.DecryptEnvelope(frame, receiveKey)
	}
	return runPortReverseClientLoop(ctx, readNext, writer, sendKey, localPort)
}

// startPortReverseListen sends the initial "listen on remotePort" request
// and waits for the machine's ack, returning an error (including the
// machine's own reported listen failure) if it can't proceed.
func startPortReverseListen(conn shellConn, sendKey, receiveKey []byte, remotePort string) error {
	env, err := common.BuildPortEnvelope(common.PortMsgControl,
		common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpListen, RemotePort: remotePort}), sendKey)
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
	_, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
	if err != nil {
		return err
	}
	var ack common.PortReverseMsg
	if err := json.Unmarshal(plaintext, &ack); err != nil {
		return err
	}
	if ack.Error != "" {
		return errors.New(ack.Error)
	}
	return nil
}

// runPortReverseClientLoop is the transport-agnostic heart of both
// runPortReverseQUIC and runPortReverseOnStream: readNext blocks for the next
// decrypted envelope, writer is this session's single wire-write owner (see
// PortReverseWriter), and sendKey encrypts what this loop sends back.
// Mirrors handleQUICPortReverseStream's machine-side
// loop.
func runPortReverseClientLoop(ctx context.Context, readNext func() (byte, []byte, error),
	writer *common.PortReverseWriter, sendKey []byte, localPort string) error {
	var mu sync.Mutex
	localConns := make(map[uint64]net.Conn)
	defer func() {
		mu.Lock()
		for _, c := range localConns {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	// The machine ends the session after ShellIdleTimeout without hearing from this side,
	// which an idle forward would otherwise be: ping it, as it pings this side.
	stopPing := make(chan struct{})
	defer close(stopPing)
	common.SafeGo(func() {
		ticker := time.NewTicker(common.ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				env, err := common.BuildPortEnvelope(common.PortMsgControl, common.MustJSON(common.PortReverseMsg{}), sendKey)
				if err != nil {
					continue
				}
				if !writer.Send(env) {
					return
				}
			}
		}
	})

	for {
		kind, plaintext, err := readNext()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		switch kind {
		case common.PortMsgControl:
			var msg common.PortReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil {
				continue
			}
			switch msg.Op {
			case common.PortRevOpConnect:
				connID := msg.ConnID
				localConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", localPort))
				if dialErr != nil {
					if env, encErr := common.BuildPortEnvelope(common.PortMsgControl,
						common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpClose, ConnID: connID}), sendKey); encErr == nil {
						writer.Send(env)
					}
					continue
				}
				mu.Lock()
				localConns[connID] = localConn
				mu.Unlock()
				common.SafeGo(func() { pumpPortReverseLocalConn(localConn, connID, sendKey, writer, &mu, localConns) })
			case common.PortRevOpClose:
				mu.Lock()
				c, ok := localConns[msg.ConnID]
				delete(localConns, msg.ConnID)
				mu.Unlock()
				if ok {
					_ = c.Close()
				}
			}
		case common.PortMsgData:
			connID, payload, ok := common.DecodeConnData(plaintext)
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
// dialed to localhost:localPort on the machine's behalf) and forwards its
// bytes back over writer, tagged with connID, until it errors or closes.
func pumpPortReverseLocalConn(localConn net.Conn, connID uint64, sendKey []byte, writer *common.PortReverseWriter,
	mu *sync.Mutex, localConns map[uint64]net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := localConn.Read(buf)
		if n > 0 {
			env, encErr := common.BuildPortEnvelope(common.PortMsgData, common.EncodeConnData(connID, buf[:n]), sendKey)
			if encErr != nil || !writer.Send(env) {
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
		if env, encErr := common.BuildPortEnvelope(common.PortMsgControl,
			common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpClose, ConnID: connID}), sendKey); encErr == nil {
			writer.Send(env)
		}
	}
}

// ParsePortMapping splits a listen:target port mapping, as desk port takes it, checking
// both are ports.
func ParsePortMapping(mapping string) (listen, target string, err error) {
	listen, target, ok := strings.Cut(mapping, ":")
	if !ok || listen == "" || target == "" {
		return "", "", fmt.Errorf("invalid port mapping: use listen:target (e.g. 8080:80)")
	}
	for _, port := range []string{listen, target} {
		n, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || n == 0 {
			return "", "", fmt.Errorf("invalid port %q: use a number from 1 to 65535", port)
		}
	}
	return listen, target, nil
}
