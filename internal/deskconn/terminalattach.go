package deskconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/muesli/cancelreader"
	"golang.org/x/term"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/terminal"
)

// TerminalDialer opens a stream to a machine's terminal sessions and makes req on it. ctx bounds
// opening it, not the stream.
type TerminalDialer func(ctx context.Context, req common.TerminalControlMsg) (
	*TerminalStream, common.TerminalControlMsg, error)

// TerminalEnd is how an attachment ended.
type TerminalEnd int

const (
	// TerminalExited is the session's shell having exited: the session is gone.
	TerminalExited TerminalEnd = iota
	// TerminalDetached is the user detaching: the session runs on.
	TerminalDetached
	// TerminalDisplaced is another attachment having taken the session over.
	TerminalDisplaced
)

const (
	// terminalReconnectTimeout is how long a lost connection is tried again.
	terminalReconnectTimeout = 5 * time.Minute
	terminalRetryMax         = 5 * time.Second
	// terminalDetachTimeout bounds waiting for the machine to end a detached attachment.
	terminalDetachTimeout = 5 * time.Second
)

// TerminalAttachment is a terminal attached to a persistent session: the user's keys go to
// the session, its output to the terminal. A lost connection is attached again, to the same
// session, restoring only what the terminal missed.
type TerminalAttachment struct {
	dial   TerminalDialer
	input  io.Reader // the user's keys
	output io.Writer // the terminal

	mu         sync.Mutex
	ts         *TerminalStream // nil while reconnecting
	cols, rows int
	detaching  bool
	detach     chan struct{} // closed on detaching

	outMu  sync.Mutex
	mirror *terminal.Mirror
}

// NewTerminalAttachment returns an attachment of a terminal of cols × rows.
func NewTerminalAttachment(dial TerminalDialer, input io.Reader, output io.Writer, cols, rows int) *TerminalAttachment {
	return &TerminalAttachment{dial: dial, input: input, output: output, cols: cols, rows: rows,
		detach: make(chan struct{}), mirror: terminal.NewMirror(cols, rows)}
}

// Resize resizes the session's terminal along with the local one.
func (a *TerminalAttachment) Resize(cols, rows int) {
	a.outMu.Lock()
	a.mirror.Resize(cols, rows)
	a.outMu.Unlock()
	a.mu.Lock()
	a.cols, a.rows = cols, rows
	ts := a.ts
	a.mu.Unlock()
	if ts != nil {
		_ = ts.SendControl(common.TerminalControlMsg{Op: common.TerminalOpSize, Cols: clampUint16(cols),
			Rows: clampUint16(rows)})
	}
}

// Run makes req, a create or an attach, and runs the attachment until the session exits,
// the user detaches or another attachment takes over. It returns how it ended, the
// session's ID and, for an exit, the shell's exit code. A refusal of req is returned as a
// *TerminalError; a connection that can't be restored, as an error with the session's ID.
// Whatever modes the session's output set on the terminal are reset as it returns.
func (a *TerminalAttachment) Run(ctx context.Context, req common.TerminalControlMsg) (
	end TerminalEnd, session string, exitCode int, err error) {
	req.Cols, req.Rows = clampUint16(a.cols), clampUint16(a.rows)
	ts, reply, err := a.dial(ctx, req)
	if err != nil {
		return 0, "", 0, err
	}
	defer a.reset()
	session, token := reply.Session, reply.Token
	// Keys read from now on have somewhere to go.
	a.mu.Lock()
	a.ts = ts
	a.mu.Unlock()
	go a.inputLoop()
	pingDone := make(chan struct{})
	defer close(pingDone)
	go a.pingLoop(pingDone)

	gapNotice := "older output of this session is no longer kept"
	for {
		seen, end, exitCode, err := a.attached(ts, reply, gapNotice)
		if err == nil {
			return end, session, exitCode, nil
		}
		gapNotice = "some output while disconnected is no longer kept"

		a.notice("connection lost; reconnecting (Ctrl+] d detaches)")
		ts, reply, err = a.reconnect(ctx, common.TerminalControlMsg{Op: common.TerminalOpAttach,
			Session: session, Token: token, Since: &seen})
		switch {
		case errors.Is(err, errTerminalDetached):
			return TerminalDetached, session, 0, nil
		case IsTerminalError(err, common.TerminalErrAttached):
			return TerminalDisplaced, session, 0, nil
		case IsTerminalError(err, common.TerminalErrNotFound):
			return 0, session, 0, fmt.Errorf("session %s has ended", session)
		case err != nil:
			return 0, session, 0, fmt.Errorf("could not reconnect to session %s, which may still be running: %w",
				session, err)
		}
		token = reply.Token
	}
}

var errTerminalDetached = errors.New("detached")

// attached runs an attachment until it ends, or its connection does (err set). seen is the
// output offset the terminal has then seen up to.
func (a *TerminalAttachment) attached(ts *TerminalStream, reply common.TerminalControlMsg, gapNotice string) (
	seen uint64, end TerminalEnd, exitCode int, err error) {
	a.mu.Lock()
	if a.detaching {
		a.mu.Unlock()
		ts.Close()
		return 0, TerminalDetached, 0, nil
	}
	a.ts = ts
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.ts = nil
		a.mu.Unlock()
		ts.Close()
	}()

	if reply.HistoryGap {
		// Scrolled into the terminal's history, where the history restored follows it: the
		// snapshot clears the screen.
		a.mu.Lock()
		rows := a.rows
		a.mu.Unlock()
		a.notice(gapNotice + string(bytes.Repeat([]byte("\r\n"), rows)))
	}
	seen, snapshot := reply.Offset, reply.Snapshot
	for {
		kind, msg, output, err := ts.Recv()
		if err != nil {
			a.mu.Lock()
			detaching := a.detaching
			a.mu.Unlock()
			if detaching {
				return seen, TerminalDetached, 0, nil
			}
			return seen, 0, 0, err
		}
		if kind == common.ShellMsgData {
			a.outMu.Lock()
			_, _ = a.output.Write(output)
			a.mirror.Write(output)
			a.outMu.Unlock()
			if n := min(snapshot, len(output)); n > 0 {
				snapshot -= n
				output = output[n:]
			}
			seen += uint64(len(output))
			continue
		}
		switch msg.Op {
		case common.TerminalOpExit:
			if msg.ExitCode != nil {
				exitCode = *msg.ExitCode
			}
			return seen, TerminalExited, exitCode, nil
		case common.TerminalOpDisplaced:
			return seen, TerminalDisplaced, 0, nil
		}
	}
}

// reconnect attaches again with req until it's attached, refused (but for the session being
// unreachable), the user detaches, or the time for it runs out.
func (a *TerminalAttachment) reconnect(ctx context.Context, req common.TerminalControlMsg) (
	*TerminalStream, common.TerminalControlMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalReconnectTimeout)
	defer cancel()
	go func() {
		select {
		case <-a.detach:
			cancel()
		case <-ctx.Done():
		}
	}()

	wait := 250 * time.Millisecond
	for {
		a.mu.Lock()
		req.Cols, req.Rows = clampUint16(a.cols), clampUint16(a.rows)
		a.mu.Unlock()
		ts, reply, err := a.dial(ctx, req)
		if err == nil {
			return ts, reply, nil
		}
		var refused *TerminalError
		if errors.As(err, &refused) && refused.Code != common.TerminalErrUnavailable {
			return nil, reply, err
		}
		select {
		case <-a.detach:
			return nil, reply, errTerminalDetached
		case <-ctx.Done():
			return nil, reply, err
		case <-time.After(wait):
		}
		wait = min(2*wait, terminalRetryMax)
	}
}

// inputLoop sends the user's keys to the session, but for the detach keys. Keys typed while
// reconnecting are dropped: the session might not get them. Input ending detaches.
func (a *TerminalAttachment) inputLoop() {
	var keys detachKeys
	buf := make([]byte, 1024)
	for {
		n, err := a.input.Read(buf)
		data, detach := keys.filter(buf[:n])
		if len(data) > 0 {
			a.send(data)
		}
		if detach || (err != nil && !errors.Is(err, cancelreader.ErrCanceled)) {
			a.startDetach()
		}
		if detach || err != nil {
			return
		}
	}
}

func (a *TerminalAttachment) send(data []byte) {
	a.mu.Lock()
	ts := a.ts
	a.mu.Unlock()
	if ts != nil {
		_ = ts.SendInput(data)
	}
}

// startDetach asks the machine to detach. Its ending the attachment's stream, then, ends it.
func (a *TerminalAttachment) startDetach() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.detaching {
		return
	}
	a.detaching = true
	close(a.detach)
	if ts := a.ts; ts != nil {
		_ = ts.SendControl(common.TerminalControlMsg{Op: common.TerminalOpDetach})
		time.AfterFunc(terminalDetachTimeout, ts.Close)
	}
}

// pingLoop keeps a quiet attachment's stream from timing out (see ShellIdleTimeout).
func (a *TerminalAttachment) pingLoop(done <-chan struct{}) {
	ticker := time.NewTicker(common.ShellPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		ts := a.ts
		a.mu.Unlock()
		if ts != nil {
			_ = ts.SendControl(common.TerminalControlMsg{Op: common.TerminalOpPing})
		}
	}
}

// notice shows the user a message about the attachment, on a line of its own.
func (a *TerminalAttachment) notice(message string) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_, _ = fmt.Fprintf(a.output, "\x1b[0m\r\n[desk] %s\r\n", message)
}

// reset returns the terminal to a shell's modes, on its normal screen.
func (a *TerminalAttachment) reset() {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	_, _ = a.output.Write(a.mirror.Reset())
}

// detachKeys finds the detach keys in what the user types: Ctrl+] then d. Ctrl+] typed twice
// sends it once; followed by any other key, it's sent along with it. Ctrl+] is also found as
// keyboard protocols a program turned on send it, as is d.
type detachKeys struct {
	prefix []byte // Ctrl+], as it was sent, when it's what was typed last
}

//nolint:gochecknoglobals // read-only tables
var (
	escapeKeys = [][]byte{
		[]byte("\x1d"),
		[]byte("\x1b[93;5u"),    // kitty keyboard protocol
		[]byte("\x1b[27;5;93~"), // xterm's modifyOtherKeys
	}
	detachKey = [][]byte{[]byte("d"), []byte("\x1b[100u")}
)

func (k *detachKeys) filter(in []byte) (out []byte, detach bool) {
	for len(in) > 0 {
		if k.prefix != nil {
			if hasKey(in, detachKey) != nil {
				return out, true
			}
			out = append(out, k.prefix...)
			escape := hasKey(in, escapeKeys)
			k.prefix = nil
			if escape != nil {
				// Typed twice: once is sent.
				in = in[len(escape):]
			}
			continue
		}
		if escape := hasKey(in, escapeKeys); escape != nil {
			k.prefix = escape
			in = in[len(escape):]
			continue
		}
		out = append(out, in[0])
		in = in[1:]
	}
	return out, false
}

// hasKey returns the key of keys in starts with.
func hasKey(in []byte, keys [][]byte) []byte {
	for _, key := range keys {
		if bytes.HasPrefix(in, key) {
			return key
		}
	}
	return nil
}

// RunTerminal attaches this terminal to a persistent session of realm's machine: req
// creates one, or attaches to one. It returns how the attachment ended and the session's ID;
// a shell exiting nonzero is an *ExitError.
func RunTerminal(ctx context.Context, mode, realm, cfgDirectory string, req common.TerminalControlMsg) (
	TerminalEnd, string, error) {
	// Ask for a standalone machine's password, or fail for missing credentials, first.
	if _, err := clientAuthenticator(realm, cfgDirectory); err != nil {
		return 0, "", err
	}
	fd := int(os.Stdin.Fd())      // #nosec
	sizeFd := int(os.Stdout.Fd()) // #nosec: see runStreamCommand
	cols, rows := defaultCols, defaultRows
	if term.IsTerminal(sizeFd) {
		var err error
		if cols, rows, err = term.GetSize(sizeFd); err != nil {
			return 0, "", fmt.Errorf("failed to get terminal size: %w", err)
		}
	}
	var stdin io.Reader = os.Stdin
	if cancelable, err := cancelreader.NewReader(os.Stdin); err == nil {
		defer func() { cancelable.Cancel(); _ = cancelable.Close() }()
		stdin = cancelable
	} else if term.IsTerminal(fd) {
		return 0, "", err
	}
	// Ctrl+C stays local until the session is attached.
	dial := TerminalDial(mode, realm, cfgDirectory)
	var oldState *term.State
	defer func() {
		if oldState != nil {
			_ = term.Restore(fd, oldState)
		}
	}()
	attach := func(ctx context.Context, req common.TerminalControlMsg) (
		*TerminalStream, common.TerminalControlMsg, error) {
		ts, reply, err := dial(ctx, req)
		if err == nil && oldState == nil && term.IsTerminal(fd) {
			if oldState, err = term.MakeRaw(fd); err != nil {
				ts.Close()
				return nil, reply, fmt.Errorf("failed to set raw mode: %w", err)
			}
		}
		return ts, reply, err
	}

	a := NewTerminalAttachment(attach, stdin, os.Stdout, cols, rows)
	if term.IsTerminal(sizeFd) {
		done := make(chan struct{})
		defer close(done)
		go watchResize(sizeFd, done, func() {
			if cols, rows, err := term.GetSize(sizeFd); err == nil {
				a.Resize(cols, rows)
			}
		})
	}
	end, session, code, err := a.Run(ctx, req)
	if err == nil && end == TerminalExited && code != 0 {
		err = &ExitError{Code: code}
	}
	return end, session, err
}
