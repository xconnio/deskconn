// Package terminal keeps persistent terminal sessions in worker processes of their own, so
// a shell and what runs in it outlive the connection that started it and the daemon too.
//
// Each session is one worker: it owns the session's PTY, its shell and the shell's process
// group, and serves them on a Unix socket in the terminals directory, next to a record of
// the session. The daemon (Manager) creates workers, finds them again from their records
// after it restarts, and relays a client's attachment to one.
package terminal

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/xconnio/deskconn/internal/common"
)

// protocolVersion is the worker protocol's version. A worker started by an older daemon can
// outlive it, so a daemon must keep talking to workers of every version it may find.
const protocolVersion = 1

// Frame kinds on a worker connection: each frame is one kind byte followed by its payload.
const (
	frameControl byte = iota // JSON message
	frameData                // raw terminal input or output
)

// Worker requests, the first message on a connection, and the control messages that follow
// an attach.
const (
	opInfo      = "info"      // describe the session, then close
	opAttach    = "attach"    // become the session's attachment
	opTerminate = "terminate" // end the session and its processes
	opSize      = "size"      // daemon → worker: resize the terminal
	opDetach    = "detach"    // daemon → worker: stop being the attachment
	opOK        = "ok"        // worker → daemon: request accepted
	opError     = "error"     // worker → daemon: request refused
	opExit      = "exit"      // worker → daemon: the shell exited, with its status
	opDetached  = "detached"  // worker → daemon: another attachment took over
)

// Error codes a worker refuses a request with.
const (
	codeAttached     = "attached"     // attached elsewhere, and no takeover was asked for
	codeUnauthorized = "unauthorized" // wrong secret
	codeEnded        = "ended"        // the shell has exited
	codeInvalid      = "invalid"      // malformed or unknown request
)

// message is every control message on a worker connection.
type message struct {
	Op       string `json:"op"`
	Version  int    `json:"version,omitempty"`
	Secret   string `json:"secret,omitempty"`   // request: the session record's secret
	Cols     uint16 `json:"cols,omitempty"`     // attach, size
	Rows     uint16 `json:"rows,omitempty"`     // attach, size
	Takeover bool   `json:"takeover,omitempty"` // attach: replace an existing attachment
	// Token, in an attach request, is the token an earlier attachment was given: presenting
	// it replaces that attachment without a takeover, as the same client reconnecting.
	// In the reply, it's the new attachment's.
	Token    string       `json:"token,omitempty"`
	Info     *SessionInfo `json:"info,omitempty"`      // ok reply to info
	ExitCode *int         `json:"exit_code,omitempty"` // exit
	Code     string       `json:"code,omitempty"`      // error
	Error    string       `json:"error,omitempty"`     // error
}

// SessionInfo describes a running session.
type SessionInfo struct {
	ID       string    `json:"id"`
	Command  string    `json:"command"`
	Created  time.Time `json:"created"`
	Attached bool      `json:"attached"`
}

func writeControl(w io.Writer, msg message) error {
	payload, err := json.Marshal(msg) //nolint:gosec // a request carries the worker's secret, over its private socket
	if err != nil {
		return err
	}
	return common.WriteFrame(w, append([]byte{frameControl}, payload...))
}

func writeData(w io.Writer, data []byte) error {
	frame := make([]byte, 1+len(data))
	frame[0] = frameData
	copy(frame[1:], data)
	return common.WriteFrame(w, frame)
}

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

// readFrame reads one frame, returning its kind and, for a control frame, the message;
// otherwise the data.
func readFrame(r io.Reader) (kind byte, msg message, data []byte, err error) {
	frame, err := common.ReadFrame(r)
	if err != nil {
		return 0, message{}, nil, err
	}
	if len(frame) == 0 {
		return 0, message{}, nil, fmt.Errorf("empty frame")
	}
	switch frame[0] {
	case frameControl:
		if err := json.Unmarshal(frame[1:], &msg); err != nil {
			return 0, message{}, nil, fmt.Errorf("invalid control message: %w", err)
		}
		return frameControl, msg, nil, nil
	case frameData:
		return frameData, message{}, frame[1:], nil
	default:
		return 0, message{}, nil, fmt.Errorf("unknown frame kind %d", frame[0])
	}
}

// readControl reads a frame that must be a control message.
func readControl(r io.Reader) (message, error) {
	kind, msg, _, err := readFrame(r)
	if err != nil {
		return message{}, err
	}
	if kind != frameControl {
		return message{}, fmt.Errorf("expected a control message")
	}
	return msg, nil
}
