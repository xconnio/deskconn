package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
)

// modeQUIC/modeP2P are the "quic"/"p2p" --mode flag values every raw-stream
// client entry point switches on. cmd/deskconn has its own ModeQUIC/ModeP2P
// with the same values, kept separate since it can't import these.
const (
	modeQUIC = "quic"
	modeP2P  = "p2p"
)

// clampUint16 clamps a terminal dimension (from term.GetSize, always
// small and non-negative in practice) into shellControlMsg's wire type.
func clampUint16(n int) uint16 {
	if n < 0 {
		return 0
	}
	if n > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(n) // #nosec G115
}

// shellConn is one live raw connection carrying shell traffic, either a
// QUIC stream or a WebRTC data channel -- the client-side counterpart to
// shell.go's shellTransport.
type shellConn interface {
	sendEnvelope(envelope []byte) error
	recvEnvelope() ([]byte, error)
	close() error
}

type quicClientShellConn struct{ stream net.Conn }

func (c *quicClientShellConn) sendEnvelope(e []byte) error   { return WriteFrame(c.stream, e) }
func (c *quicClientShellConn) recvEnvelope() ([]byte, error) { return ReadFrame(c.stream) }
func (c *quicClientShellConn) close() error                  { return c.stream.Close() }

type p2pClientShellConn struct {
	channel *webrtc.DataChannel
	msgCh   chan []byte
	closed  <-chan struct{}
}

// newP2PClientShellConn takes over channel's OnMessage handler -- callers
// must complete key exchange first and pass webrtcBackpressure's existing
// closed channel rather than calling webrtcBackpressure again, which would
// replace its OnClose/OnError registration.
func newP2PClientShellConn(channel *webrtc.DataChannel, closed <-chan struct{}) *p2pClientShellConn {
	c := &p2pClientShellConn{channel: channel, msgCh: make(chan []byte, 8), closed: closed}
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case c.msgCh <- msg.Data:
		case <-closed:
		}
	})
	return c
}

func (c *p2pClientShellConn) sendEnvelope(e []byte) error { return c.channel.Send(e) }
func (c *p2pClientShellConn) recvEnvelope() ([]byte, error) {
	select {
	case data := <-c.msgCh:
		return data, nil
	case <-c.closed:
		return nil, io.ErrClosedPipe
	}
}
func (c *p2pClientShellConn) close() error { return c.channel.Close() }

func sendShellControl(conn shellConn, sendKey []byte, msg shellControlMsg) error {
	plaintext, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	envelope, err := buildShellEnvelope(shellMsgControl, plaintext, sendKey)
	if err != nil {
		return err
	}
	return conn.sendEnvelope(envelope)
}

func recvShellAck(conn shellConn, receiveKey []byte) (shellControlMsg, error) {
	envelope, err := conn.recvEnvelope()
	if err != nil {
		return shellControlMsg{}, err
	}
	kind, plaintext, err := DecryptEnvelope(envelope, receiveKey)
	if err != nil {
		return shellControlMsg{}, err
	}
	if kind != shellMsgControl {
		return shellControlMsg{}, fmt.Errorf("unexpected ack kind %d", kind)
	}
	var msg shellControlMsg
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return shellControlMsg{}, err
	}
	if msg.Error != "" {
		return shellControlMsg{}, errors.New(msg.Error)
	}
	return msg, nil
}

// shellHandshakeResult is what dialing and completing the initial
// size/migrate handshake on a shell connection produces. token is only set
// for a brand-new session (shellOpSize) -- the migration token this
// connection can later be handed off from. cleanup releases the underlying
// QUIC connection or PeerConnection and must be called exactly once.
type shellHandshakeResult struct {
	conn                shellConn
	sendKey, receiveKey []byte
	shellID, token      string
	cleanup             func()
}

func dialShellQUIC(ctx context.Context, realm, cfgDirectory string,
	ctrl shellControlMsg) (*shellHandshakeResult, error) {
	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = quicSess.Connection().Close() }

	stream, err := quicSess.OpenStream()
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpShell}); err != nil {
		cleanup()
		return nil, err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		cleanup()
		return nil, err
	}

	conn := &quicClientShellConn{stream: stream}
	if err := sendShellControl(conn, sendKey, ctrl); err != nil {
		cleanup()
		return nil, err
	}
	ack, err := recvShellAck(conn, receiveKey)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &shellHandshakeResult{
		conn: conn, sendKey: sendKey, receiveKey: receiveKey,
		shellID: ack.ShellID, token: ack.Token, cleanup: cleanup,
	}, nil
}

func dialShellP2P(ctx context.Context, realm, cfgDirectory string,
	ctrl shellControlMsg) (*shellHandshakeResult, error) {
	p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = p2pSess.Close() }

	channel, err := openP2PChannel(p2pSess, ShellChannelLabel)
	if err != nil {
		cleanup()
		return nil, err
	}
	closed, _ := WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		cleanup()
		return nil, err
	}

	conn := newP2PClientShellConn(channel, closed)
	if err := sendShellControl(conn, sendKey, ctrl); err != nil {
		cleanup()
		return nil, err
	}
	ack, err := recvShellAck(conn, receiveKey)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &shellHandshakeResult{
		conn: conn, sendKey: sendKey, receiveKey: receiveKey,
		shellID: ack.ShellID, token: ack.Token, cleanup: cleanup,
	}, nil
}

// activeShellConn is the connection the stdin/resize/output loops below are
// currently using, swappable in place by a successful background migration.
type activeShellConn struct {
	mu                  sync.Mutex
	conn                shellConn
	sendKey, receiveKey []byte
	cleanup             func()
}

func (a *activeShellConn) set(r *shellHandshakeResult) {
	a.mu.Lock()
	a.conn, a.sendKey, a.receiveKey, a.cleanup = r.conn, r.sendKey, r.receiveKey, r.cleanup
	a.mu.Unlock()
}

func (a *activeShellConn) get() (shellConn, []byte, []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn, a.sendKey, a.receiveKey
}

func (a *activeShellConn) cleanupCurrent() {
	a.mu.Lock()
	cleanup := a.cleanup
	a.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func shellStdinLoop(active *activeShellConn) {
	buf := make([]byte, 1024)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		conn, sendKey, _ := active.get()
		envelope, err := buildShellEnvelope(shellMsgData, buf[:n], sendKey)
		if err != nil {
			continue
		}
		_ = conn.sendEnvelope(envelope)
	}
}

func shellResizeLoop(active *activeShellConn, fd int) {
	watchResize(fd, func() {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			return
		}
		conn, sendKey, _ := active.get()
		msg := shellControlMsg{Op: shellOpSize, Cols: clampUint16(cols), Rows: clampUint16(rows)}
		plaintext, err := json.Marshal(msg)
		if err != nil {
			return
		}
		envelope, err := buildShellEnvelope(shellMsgControl, plaintext, sendKey)
		if err != nil {
			return
		}
		_ = conn.sendEnvelope(envelope)
	})
}

// shellPingInterval is how often shellPingLoop sends shellOpPing while
// otherwise idle. Must stay comfortably under shellIdleTimeout.
const shellPingInterval = 10 * time.Second

// shellPingLoop keeps the active connection producing traffic even when the
// user isn't typing or resizing, so the server's idle-connection deadline
// (shellIdleTimeout) doesn't mistake a quiet session for a dead one -- see
// its doc comment for why that deadline exists at all.
func shellPingLoop(active *activeShellConn) {
	ticker := time.NewTicker(shellPingInterval)
	defer ticker.Stop()
	for range ticker.C {
		conn, sendKey, _ := active.get()
		_ = sendShellControl(conn, sendKey, shellControlMsg{Op: shellOpPing})
	}
}

// shellReadLoop writes decrypted PTY output to stdout until the active
// connection ends. A migration swapping active out from under it isn't
// treated as the session ending -- it switches to the new connection
// instead.
func shellReadLoop(active *activeShellConn) error {
	conn, _, receiveKey := active.get()
	for {
		envelope, err := conn.recvEnvelope()
		if err != nil {
			newConn, _, newReceiveKey := active.get()
			if newConn != conn {
				conn, receiveKey = newConn, newReceiveKey
				continue
			}
			return err
		}
		kind, plaintext, err := DecryptEnvelope(envelope, receiveKey)
		if err != nil {
			continue
		}
		if kind == shellMsgData {
			_, _ = os.Stdout.Write(plaintext)
		}
	}
}

// runStreamCommand is the shared connect-and-run loop behind RunShell and
// RunExec: ctrl seeds the initial control message (RunExec sets
// Command/Args; RunShell leaves them empty for an interactive bash shell),
// and Op/Cols/Rows are filled in here. mode selects the connection policy:
//   - "quic": QUIC only, no upgrade attempt.
//   - "p2p": P2P only, no QUIC fast-start.
//   - "" (default): fast-start on QUIC so the prompt appears immediately,
//     then attempt a background P2P upgrade and live-migrate the running
//     PTY onto it if it succeeds. A failed upgrade is never surfaced as an
//     error; staying on QUIC is fine.
func runStreamCommand(ctx context.Context, mode, realm, cfgDirectory string, ctrl shellControlMsg) error {
	fd := int(os.Stdin.Fd()) // #nosec
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, oldState) }()

	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return fmt.Errorf("failed to get terminal size: %w", err)
	}
	ctrl.Op, ctrl.Cols, ctrl.Rows = shellOpSize, clampUint16(cols), clampUint16(rows)

	var primary *shellHandshakeResult
	if mode == modeP2P {
		primary, err = dialShellP2P(ctx, realm, cfgDirectory, ctrl)
	} else {
		primary, err = dialShellQUIC(ctx, realm, cfgDirectory, ctrl)
	}
	if err != nil {
		return err
	}

	active := &activeShellConn{}
	active.set(primary)
	defer active.cleanupCurrent()

	go shellStdinLoop(active)
	go shellResizeLoop(active, fd)
	go shellPingLoop(active)

	if mode == "" {
		go func() {
			migrateCtrl := shellControlMsg{
				Op: shellOpMigrate, OldID: primary.shellID, Token: primary.token, AuthID: ctrl.AuthID,
			}
			upgrade, err := dialShellP2P(ctx, realm, cfgDirectory, migrateCtrl)
			if err != nil {
				log.Debugf("stream: background P2P upgrade failed, staying on QUIC: %v", err)
				return
			}
			active.set(upgrade)
			_ = primary.conn.close()
			primary.cleanup()
			log.Debugf("stream: migrated live session %s from QUIC to P2P", primary.shellID)
		}()
	}

	// shellReadLoop's error just means the connection closed, which is the
	// normal way a session ends (the device closes it once the PTY exits) --
	// not a failure worth surfacing.
	_ = shellReadLoop(active)
	return nil
}

func RunShell(ctx context.Context, mode, realm, cfgDirectory string) error {
	authID, _, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return err
	}
	return runStreamCommand(ctx, mode, realm, cfgDirectory, shellControlMsg{AuthID: authID})
}

// RunExec is the client entry point for anything that runs one command on a
// device's PTY and exits when it finishes (deskconn exec, file ls, ai
// resume) -- as opposed to RunShell's open-ended interactive session.
func RunExec(ctx context.Context, mode, realm, cfgDirectory string, commandWithArgs []string) error {
	var command string
	var args []string
	if len(commandWithArgs) > 0 {
		command, args = commandWithArgs[0], commandWithArgs[1:]
	}
	return runStreamCommand(ctx, mode, realm, cfgDirectory, shellControlMsg{Command: command, Args: args})
}
