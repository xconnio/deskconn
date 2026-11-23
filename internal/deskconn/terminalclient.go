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
	writeMu             sync.Mutex
	closeOnce           sync.Once
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
