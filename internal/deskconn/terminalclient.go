package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
)

// TerminalError is a machine refusing a terminal request, with its common.TerminalErr code.
type TerminalError struct {
	Code    string
	Message string
}

func (e *TerminalError) Error() string { return e.Message }

// IsTerminalError reports whether err is a machine refusing a terminal request with code.
func IsTerminalError(err error, code string) bool {
	var terminalErr *TerminalError
	return errors.As(err, &terminalErr) && terminalErr.Code == code
}

// TerminalStream is a stream to a machine's persistent terminal sessions: after the request
// it was opened with, an attachment's input, output and control messages.
type TerminalStream struct {
	stream              net.Conn
	sendKey, receiveKey []byte
	cleanup             func()
	// toDirect, for a stream that took the Hub while a direct path could follow, returns once
	// a stream opened again would go direct.
	toDirect  func(ctx context.Context) error
	writeMu   sync.Mutex
	closeOnce sync.Once
}

// MoveWhen has an attachment on this stream move to a new one once ready returns nil: ready
// returns once a stream opened again would take a direct path.
func (ts *TerminalStream) MoveWhen(ready func(ctx context.Context) error) {
	ts.toDirect = ready
}

// StartTerminal makes req on stream, an open terminal stream, and returns it with the
// machine's reply. cleanup releases the stream, and is called on failure.
func StartTerminal(ctx context.Context, stream net.Conn, cleanup func(), req common.TerminalControlMsg) (
	*TerminalStream, common.TerminalControlMsg, error) {
	ts := &TerminalStream{stream: stream, cleanup: cleanup}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	reply, err := ts.handshake(req)
	stop()
	if err != nil {
		ts.Close()
		return nil, common.TerminalControlMsg{}, err
	}
	_ = stream.SetDeadline(time.Time{})
	return ts, reply, nil
}

func (ts *TerminalStream) handshake(req common.TerminalControlMsg) (common.TerminalControlMsg, error) {
	var err error
	if ts.sendKey, ts.receiveKey, err = StreamClientKeyExchange(ts.stream); err != nil {
		return common.TerminalControlMsg{}, fmt.Errorf("terminal key exchange: %w", err)
	}
	if err := ts.SendControl(req); err != nil {
		return common.TerminalControlMsg{}, err
	}
	kind, msg, _, err := ts.Recv()
	if err != nil {
		return common.TerminalControlMsg{}, fmt.Errorf("terminal reply: %w", err)
	}
	if kind != common.ShellMsgControl {
		return common.TerminalControlMsg{}, errors.New("unexpected terminal reply")
	}
	if msg.Error != "" || msg.Code != "" {
		return common.TerminalControlMsg{}, &TerminalError{Code: msg.Code, Message: msg.Error}
	}
	return msg, nil
}

// SendControl sends a control message.
func (ts *TerminalStream) SendControl(msg common.TerminalControlMsg) error {
	plaintext, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return ts.send(common.ShellMsgControl, plaintext)
}

// SendInput sends terminal input.
func (ts *TerminalStream) SendInput(input []byte) error {
	return ts.send(common.ShellMsgData, input)
}

func (ts *TerminalStream) send(kind byte, plaintext []byte) error {
	envelope, err := common.BuildShellEnvelope(kind, plaintext, ts.sendKey)
	if err != nil {
		return err
	}
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return common.WriteFrame(ts.stream, envelope)
}

// Recv returns the next message: a control message, or terminal output.
func (ts *TerminalStream) Recv() (kind byte, msg common.TerminalControlMsg, output []byte, err error) {
	frame, err := common.ReadFrame(ts.stream)
	if err != nil {
		return 0, msg, nil, err
	}
	kind, plaintext, err := common.DecryptEnvelope(frame, ts.receiveKey)
	if err != nil {
		return 0, msg, nil, err
	}
	if kind == common.ShellMsgData {
		return kind, msg, plaintext, nil
	}
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return 0, msg, nil, fmt.Errorf("invalid terminal message: %w", err)
	}
	return kind, msg, nil, nil
}

// Close closes the stream. For an attachment, that detaches it: the session runs on.
func (ts *TerminalStream) Close() {
	ts.closeOnce.Do(ts.cleanup)
}

// ErrTerminalUnsupported is a machine without persistent terminal sessions: its DeskConn is
// older, or its terminal app is disabled.
var ErrTerminalUnsupported = errors.New("the machine has no persistent terminal sessions")

// openTerminalStream opens a stream to realm's machine's terminal sessions, over the
// connection mode selects, as runStreamCommand does. For a stream through the daemon over the
// Hub, toDirect returns once the daemon has a direct path, for the attachment to move to it.
func openTerminalStream(ctx context.Context, mode, realm, cfgDirectory string) (
	stream net.Conn, release func(), toDirect func(context.Context) error, err error) {
	var session common.StreamSession
	transport := "QUIC"
	switch mode {
	case ModeDirect:
		p2p, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
		if err != nil {
			return nil, nil, nil, err
		}
		session, release, transport = p2p, func() { _ = p2p.Close() }, "WebRTC"
	default:
		if mode != ModeRouted {
			daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
			if err == nil {
				session, release, transport = daemon, func() { _ = daemon.Close() }, "local daemon"
				if !daemon.P2P() {
					toDirect = func(ctx context.Context) error { return awaitDaemonDirect(ctx, realm, cfgDirectory) }
				}
				break
			} else if !errors.Is(err, ErrDaemonUnavailable) {
				return nil, nil, nil, err
			}
		}
		quic, err := connectivity.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return nil, nil, nil, err
		}
		session, release = quic, func() { _ = quic.Connection().Close() }
	}
	stream, err = common.OpenRPCStream(ctx, session, common.ProcedureTerminalStream, false)
	if err != nil {
		release()
		var application *xconn.Error
		if errors.As(err, &application) && application.URI == common.ErrNoSuchProcedure {
			return nil, nil, nil, ErrTerminalUnsupported
		}
		return nil, nil, nil, fmt.Errorf("open terminal RPC %s for realm %q via %s: %w",
			common.ProcedureTerminalStream, realm, transport, err)
	}
	return stream, release, toDirect, nil
}

// awaitDaemonDirect returns once the daemon's connection to realm's machine has a direct path,
// checking less often the longer it hasn't.
func awaitDaemonDirect(ctx context.Context, realm, cfgDirectory string) error {
	wait := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		daemon, err := DialDaemonStreams(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		direct := daemon.P2P()
		_ = daemon.Close()
		if direct {
			return nil
		}
		wait = min(2*wait, terminalDirectCheckMax)
	}
}

// TerminalDial is the TerminalDialer of realm's machine, over the connection mode selects.
// Failing to reach the machine is a *ConnectionError.
func TerminalDial(mode, realm, cfgDirectory string) TerminalDialer {
	return func(ctx context.Context, req common.TerminalControlMsg) (
		*TerminalStream, common.TerminalControlMsg, error) {
		// ctx and a timeout bound setup; the stream, once open, lasts until it's closed.
		setupCtx, finishSetup, cancel := shellSetupContext(context.WithoutCancel(ctx), shellSetupTimeout)
		stopAbort := context.AfterFunc(ctx, cancel)
		stream, release, toDirect, err := openTerminalStream(setupCtx, mode, realm, cfgDirectory)
		var ts *TerminalStream
		var reply common.TerminalControlMsg
		if err == nil {
			ts, reply, err = StartTerminal(setupCtx, stream, func() { _ = stream.Close(); release(); cancel() }, req)
		}
		if err == nil {
			ts.MoveWhen(toDirect)
		}
		aborted := !stopAbort()
		if setupErr := finishSetup(); (setupErr != nil || aborted) && err == nil {
			ts.Close()
			err = fmt.Errorf("terminal connection setup: %w", context.Cause(setupCtx))
		}
		if err != nil {
			cancel()
			var refused *TerminalError
			if errors.As(err, &refused) || errors.Is(err, ErrTerminalUnsupported) {
				return nil, reply, err
			}
			return nil, reply, &ConnectionError{Err: err}
		}
		return ts, reply, nil
	}
}

// TerminalRequest makes req, a list or a terminate, of realm's machine.
func TerminalRequest(ctx context.Context, mode, realm, cfgDirectory string, req common.TerminalControlMsg) (
	common.TerminalControlMsg, error) {
	if _, err := clientAuthenticator(realm, cfgDirectory); err != nil {
		return common.TerminalControlMsg{}, err
	}
	ts, reply, err := TerminalDial(mode, realm, cfgDirectory)(ctx, req)
	if err != nil {
		return reply, err
	}
	ts.Close()
	return reply, nil
}
