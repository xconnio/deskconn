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

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// modeQUIC/modeP2P are the "quic"/"p2p" --mode flag values every raw-stream
// client entry point switches on. cmd/desk has its own ModeQUIC/ModeP2P
// with the same values, kept separate since it can't import these.
const (
	modeQUIC = "quic"
	modeP2P  = "p2p"
)

// clampUint16 clamps a terminal dimension (from term.GetSize, always
// small and non-negative in practice) into ShellControlMsg's wire type.
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

func (c *quicClientShellConn) sendEnvelope(e []byte) error   { return common.WriteFrame(c.stream, e) }
func (c *quicClientShellConn) recvEnvelope() ([]byte, error) { return common.ReadFrame(c.stream) }
func (c *quicClientShellConn) close() error                  { return c.stream.Close() }

type p2pClientShellConn struct {
	channel common.MessageChannel
	msgCh   chan []byte
	closed  <-chan struct{}
}

// newP2PClientShellConn takes over channel's OnMessage handler -- callers
// must complete key exchange first and pass webrtcBackpressure's existing
// closed channel rather than calling webrtcBackpressure again, which would
// replace its OnClose/OnError registration.
func newP2PClientShellConn(channel common.MessageChannel, closed <-chan struct{}) *p2pClientShellConn {
	c := &p2pClientShellConn{channel: channel, msgCh: make(chan []byte, 8), closed: closed}
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		common.DeliverUnlessClosed(c.msgCh, msg.Data, closed)
	})
	return c
}

func (c *p2pClientShellConn) sendEnvelope(e []byte) error { return c.channel.Send(e) }
func (c *p2pClientShellConn) recvEnvelope() ([]byte, error) {
	select {
	case data := <-c.msgCh:
		return data, nil
	case <-c.closed:
		// Whatever arrived before the close still comes first.
		select {
		case data := <-c.msgCh:
			return data, nil
		default:
			return nil, io.ErrClosedPipe
		}
	}
}
func (c *p2pClientShellConn) close() error { return c.channel.Close() }

func sendShellControl(conn shellConn, sendKey []byte, msg common.ShellControlMsg) error {
	plaintext, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	envelope, err := common.BuildShellEnvelope(common.ShellMsgControl, plaintext, sendKey)
	if err != nil {
		return err
	}
	return conn.sendEnvelope(envelope)
}

func recvShellAck(conn shellConn, receiveKey []byte) (common.ShellControlMsg, error) {
	envelope, err := conn.recvEnvelope()
	if err != nil {
		return common.ShellControlMsg{}, err
	}
	kind, plaintext, err := common.DecryptEnvelope(envelope, receiveKey)
	if err != nil {
		return common.ShellControlMsg{}, err
	}
	if kind != common.ShellMsgControl {
		return common.ShellControlMsg{}, fmt.Errorf("unexpected ack kind %d", kind)
	}
	var msg common.ShellControlMsg
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return common.ShellControlMsg{}, err
	}
	if msg.Error != "" {
		return common.ShellControlMsg{}, errors.New(msg.Error)
	}
	return msg, nil
}

// shellHandshakeResult is what dialing and completing the initial
// size/migrate handshake on a shell connection produces. token is only set
// for a brand-new session (ShellOpSize) -- the migration token this
// connection can later be handed off from. cleanup releases the underlying
// QUIC connection or PeerConnection and must be called exactly once.
type shellHandshakeResult struct {
	conn                shellConn
	sendKey, receiveKey []byte
	shellID, token      string
	cleanup             func()
}

func dialShellQUIC(ctx context.Context, realm, cfgDirectory string,
	ctrl common.ShellControlMsg) (*shellHandshakeResult, error) {
	quicSess, err := common.ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	return shellHandshakeQUIC(quicSess, realm, ctrl, func() { _ = quicSess.Connection().Close() })
}

// shellHandshakeQUIC opens the shell on a stream of sess. release frees sess: it's
// called if the handshake fails, and otherwise becomes part of the result's cleanup.
func shellHandshakeQUIC(sess xconn.MultiplexedSession, realm string, ctrl common.ShellControlMsg,
	release func()) (*shellHandshakeResult, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		release()
		return nil, err
	}
	cleanup := func() { _ = stream.Close(); release() }
	if err := common.WriteMsg(stream, common.RoutingFrame{Realm: realm, Op: common.FSOpShell}); err != nil {
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
	ctrl common.ShellControlMsg) (*shellHandshakeResult, error) {
	p2pSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	return shellHandshakeP2P(P2PChannels(p2pSess), ctrl, func() { _ = p2pSess.Close() })
}

// shellHandshakeP2P is shellHandshakeQUIC over a data channel.
func shellHandshakeP2P(p2pSess ChannelOpener, ctrl common.ShellControlMsg,
	release func()) (*shellHandshakeResult, error) {
	channel, err := p2pSess.OpenMessageChannel(common.ShellChannelLabel)
	if err != nil {
		release()
		return nil, err
	}
	cleanup := func() { _ = channel.Close(); release() }
	closed, _ := common.WebrtcBackpressure(channel)
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

// dialShellDaemon opens the shell on deskconnd's persistent connection.
func dialShellDaemon(ds *DaemonStreams, realm string, ctrl common.ShellControlMsg) (*shellHandshakeResult, error) {
	release := func() { _ = ds.Close() }
	if ds.P2P() {
		return shellHandshakeP2P(ds, ctrl, release)
	}
	return shellHandshakeQUIC(ds, realm, ctrl, release)
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
		envelope, err := common.BuildShellEnvelope(common.ShellMsgData, buf[:n], sendKey)
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
		msg := common.ShellControlMsg{Op: common.ShellOpSize, Cols: clampUint16(cols), Rows: clampUint16(rows)}
		plaintext, err := json.Marshal(msg)
		if err != nil {
			return
		}
		envelope, err := common.BuildShellEnvelope(common.ShellMsgControl, plaintext, sendKey)
		if err != nil {
			return
		}
		_ = conn.sendEnvelope(envelope)
	})
}

// shellPingLoop keeps the active connection producing traffic even when the
// user isn't typing or resizing, so the server's idle-connection deadline
// (ShellIdleTimeout) doesn't mistake a quiet session for a dead one -- see
// its doc comment for why that deadline exists at all.
func shellPingLoop(active *activeShellConn) {
	ticker := time.NewTicker(common.ShellPingInterval)
	defer ticker.Stop()
	for range ticker.C {
		conn, sendKey, _ := active.get()
		_ = sendShellControl(conn, sendKey, common.ShellControlMsg{Op: common.ShellOpPing})
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
		kind, plaintext, err := common.DecryptEnvelope(envelope, receiveKey)
		if err != nil {
			continue
		}
		if kind == common.ShellMsgData {
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
//   - "" (default): use deskconnd's persistent connection: P2P if it has it,
//     otherwise QUIC, migrating the running PTY to P2P once the connection
//     upgrades. Without deskconnd, fast-start on a QUIC connection of its own
//     and migrate to P2P in the background the same way.
func runStreamCommand(ctx context.Context, mode, realm, cfgDirectory string, ctrl common.ShellControlMsg) error {
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
	ctrl.Op, ctrl.Cols, ctrl.Rows = common.ShellOpSize, clampUint16(cols), clampUint16(rows)

	var primary *shellHandshakeResult
	var daemon *DaemonStreams
	switch mode {
	case modeP2P:
		primary, err = dialShellP2P(ctx, realm, cfgDirectory, ctrl)
	case modeQUIC:
		primary, err = dialShellQUIC(ctx, realm, cfgDirectory, ctrl)
	default:
		daemon, err = DialDaemonStreams(ctx, realm, cfgDirectory)
		switch {
		case err == nil:
			primary, err = dialShellDaemon(daemon, realm, ctrl)
		case errors.Is(err, ErrDaemonUnavailable):
			primary, err = dialShellQUIC(ctx, realm, cfgDirectory, ctrl)
		}
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

	migrateCtrl := common.ShellControlMsg{
		Op: common.ShellOpMigrate, OldID: primary.shellID, Token: primary.token, AuthID: ctrl.AuthID,
	}
	switch {
	case daemon != nil && !daemon.P2P():
		go migrateShell(active, primary, func() (*shellHandshakeResult, error) {
			p2p, err := awaitDaemonP2P(ctx, realm, cfgDirectory)
			if err != nil {
				return nil, err
			}
			return dialShellDaemon(p2p, realm, migrateCtrl)
		})
	case daemon == nil && mode == "":
		go migrateShell(active, primary, func() (*shellHandshakeResult, error) {
			return dialShellP2P(ctx, realm, cfgDirectory, migrateCtrl)
		})
	}

	// shellReadLoop's error just means the connection closed, which is the
	// normal way a session ends (the device closes it once the PTY exits) --
	// not a failure worth surfacing.
	_ = shellReadLoop(active)
	return nil
}

// migrateShell moves the running session from primary onto the P2P connection dial
// returns. If dial fails the session just stays on QUIC.
func migrateShell(active *activeShellConn, primary *shellHandshakeResult,
	dial func() (*shellHandshakeResult, error)) {
	upgrade, err := dial()
	if err != nil {
		log.Debugf("stream: background P2P upgrade failed, staying on QUIC: %v", err)
		return
	}
	active.set(upgrade)
	_ = primary.conn.close()
	primary.cleanup()
	log.Debugf("stream: migrated live session %s from QUIC to P2P", primary.shellID)
}

// awaitDaemonP2P waits, for up to P2PRequestTimeout, for deskconnd's connection to
// realm's device to upgrade to P2P.
func awaitDaemonP2P(ctx context.Context, realm, cfgDirectory string) (*DaemonStreams, error) {
	ctx, cancel := context.WithTimeout(ctx, common.P2PRequestTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the connection to the device did not upgrade to P2P")
		case <-ticker.C:
		}
		daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		if daemon.P2P() {
			return daemon, nil
		}
		_ = daemon.Close()
	}
}

func RunShell(ctx context.Context, mode, realm, cfgDirectory string) error {
	authenticator, err := clientAuthenticator(realm, cfgDirectory)
	if err != nil {
		return err
	}
	return runStreamCommand(ctx, mode, realm, cfgDirectory, common.ShellControlMsg{AuthID: authenticator.AuthID()})
}

// RunExec is the client entry point for anything that runs one command on a
// device's PTY and exits when it finishes (desk exec, file ls, ai
// resume) -- as opposed to RunShell's open-ended interactive session.
func RunExec(ctx context.Context, mode, realm, cfgDirectory string, commandWithArgs []string) error {
	var command string
	var args []string
	if len(commandWithArgs) > 0 {
		command, args = commandWithArgs[0], commandWithArgs[1:]
	}
	return runStreamCommand(ctx, mode, realm, cfgDirectory, common.ShellControlMsg{Command: command, Args: args})
}
