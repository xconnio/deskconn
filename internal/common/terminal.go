package common

import "time"

// terminalOp is TerminalControlMsg's discriminator.
type terminalOp string

// The persistent terminal protocol (ProcedureTerminalStream). It's carried in the same
// encrypted envelopes as the shell protocol: ShellMsgControl for a JSON TerminalControlMsg,
// ShellMsgData for terminal input and output.
//
// A stream's first message is its request: TerminalOpCreate, TerminalOpAttach, TerminalOpList
// or TerminalOpTerminate. The machine answers with one TerminalControlMsg: Error and Code set
// if it refused, otherwise the result. List and terminate end there. Create and attach go on
// as an attachment: output, then TerminalOpExit or TerminalOpDisplaced, from the machine;
// input, TerminalOpSize, TerminalOpPing and TerminalOpDetach from the client. The stream
// closing without either leaves the session running, detached, like TerminalOpDetach.
//
// An attachment's output starts with Snapshot bytes that bring a terminal to the session's
// screen, with its history, as it was at output offset Offset; the session's output from
// there follows. A client counting the output after the snapshot knows the offset it has
// seen up to: attaching again with it as Since restores only the history it missed.
const (
	// TerminalOpCreate starts a new session, with Cols, Rows and Dir, and attaches to it.
	TerminalOpCreate terminalOp = "create"
	// TerminalOpAttach attaches to Session. It refuses a session attached elsewhere unless
	// Takeover is set, or Token is the token that attachment was given: the same client
	// reconnecting. It never creates a session. Since leaves out of the history restored
	// what the client has already.
	TerminalOpAttach terminalOp = "attach"
	// TerminalOpList lists the caller's sessions in the reply's Sessions.
	TerminalOpList terminalOp = "list"
	// TerminalOpTerminate ends Session and its processes.
	TerminalOpTerminate terminalOp = "terminate"
	// TerminalOpSize resizes the attached session's terminal to Cols and Rows.
	TerminalOpSize terminalOp = "size"
	// TerminalOpPing keeps an idle attachment's stream alive (see ShellIdleTimeout).
	TerminalOpPing terminalOp = "ping"
	// TerminalOpDetach ends the attachment, leaving the session running.
	TerminalOpDetach terminalOp = "detach"
	// TerminalOpExit is the machine's last message to an attachment once the shell exited,
	// with ExitCode. The session is gone.
	TerminalOpExit terminalOp = "exit"
	// TerminalOpDisplaced is the machine's last message to an attachment another one took
	// over. The session runs on.
	TerminalOpDisplaced terminalOp = "displaced"
)

// Codes a terminal request is refused with, in TerminalControlMsg.Code.
const (
	// TerminalErrNotFound is a session that doesn't exist, has ended, or isn't the caller's.
	TerminalErrNotFound = "not_found"
	// TerminalErrAttached is a session attached elsewhere, without a takeover.
	TerminalErrAttached = "attached"
	// TerminalErrUnavailable is a session the machine can't reach.
	TerminalErrUnavailable = "unavailable"
	// TerminalErrDisabled is the terminal app being disabled on the machine.
	TerminalErrDisabled = "disabled"
	// TerminalErrInvalid is a malformed request.
	TerminalErrInvalid = "invalid"
	// TerminalErrLimit is a new session refused, as the machine's sessions have all the
	// output history it allows them.
	TerminalErrLimit = "limit"
	// TerminalErrFailed is any other failure, such as the shell not starting.
	TerminalErrFailed = "failed"
)

// TerminalControlMsg is every control message of the persistent terminal protocol.
type TerminalControlMsg struct {
	Op       terminalOp `json:"op,omitempty"`
	Session  string     `json:"session,omitempty"`  // attach, terminate; the reply to create and attach
	Cols     uint16     `json:"cols,omitempty"`     // create, attach, size
	Rows     uint16     `json:"rows,omitempty"`     // create, attach, size
	Dir      string     `json:"dir,omitempty"`      // create: where to start; empty means home
	Takeover bool       `json:"takeover,omitempty"` // attach
	// Token, in the reply to create and attach, identifies the attachment; in an attach, it
	// presents an earlier one's, to replace it without a takeover.
	Token    string            `json:"token,omitempty"`
	Sessions []TerminalSession `json:"sessions,omitempty"`  // the reply to list
	ExitCode *int              `json:"exit_code,omitempty"` // exit
	Code     string            `json:"code,omitempty"`      // a refusal's TerminalErr code
	Error    string            `json:"error,omitempty"`     // a refusal's message
	// Since, in an attach, is the output offset the client has seen up to.
	Since *uint64 `json:"since,omitempty"`
	// Offset and Snapshot, in the reply to create and attach, are the output offset of the
	// snapshot and its length, which the attachment's output starts with.
	Offset   uint64 `json:"offset,omitempty"`
	Snapshot int    `json:"snapshot,omitempty"`
	// HistoryGap, in the reply to create and attach, tells that some of the history the
	// snapshot would restore is no longer kept.
	HistoryGap bool `json:"history_gap,omitempty"`
}

// TerminalSession describes a running session.
type TerminalSession struct {
	ID       string    `json:"id"`
	Command  string    `json:"command"`
	Created  time.Time `json:"created"`
	Attached bool      `json:"attached"`
}
