package userd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/terminal"
)

// TerminalWorkerCommand is the hidden command a daemon's executable runs a terminal worker
// as (see terminal.RunWorker).
const TerminalWorkerCommand = "terminal-worker"

// terminalRequestTimeout bounds a terminal request before the stream becomes an attachment.
const terminalRequestTimeout = 30 * time.Second

// terminalDetachTimeout bounds waiting for a session to let go of a detaching attachment.
const terminalDetachTimeout = 5 * time.Second

func newTerminalManager(cfgDirectory string) *terminal.Manager {
	return terminal.NewManager(filepath.Join(cfgDirectory, "terminals"), TerminalWorkerCommand)
}

// SetTerminalHistoryLimits sets the output history each new persistent terminal session
// keeps, and that they may all keep (see terminal.Manager.SetHistoryLimits).
func (d *Deskconn) SetTerminalHistoryLimits(perSession, total int) {
	d.terminals.SetHistoryLimits(perSession, total)
}

// terminalOwner is the owner caller's sessions are recorded with: its authenticated
// identity, scoped to how this daemon authenticates callers, as a standalone user's name
// and an account's ID are different namespaces.
func (d *Deskconn) terminalOwner(caller string) string {
	if d.standaloneUsers != nil {
		return "standalone:" + caller
	}
	return "account:" + caller
}

// handleTerminalStream serves one persistent terminal request (see TerminalControlMsg)
// made by caller, the authenticated identity of its session.
func (d *Deskconn) handleTerminalStream(stream net.Conn, caller string) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
	sendKey, receiveKey, err := streamServerKeyExchange(stream)
	if err != nil {
		return
	}
	ts := &terminalStream{stream: stream, sendKey: sendKey, receiveKey: receiveKey}
	kind, payload, err := ts.recv()
	if err != nil {
		return
	}
	var req common.TerminalControlMsg
	if kind != common.ShellMsgControl || json.Unmarshal(payload, &req) != nil {
		_ = ts.refuse(common.TerminalErrInvalid, "expected a terminal request")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), terminalRequestTimeout)
	defer cancel()
	owner := d.terminalOwner(caller)
	switch req.Op {
	case common.TerminalOpList:
		sessions, err := d.terminals.List(ctx, owner)
		if err != nil {
			_ = ts.refuseErr(err)
			return
		}
		list := make([]common.TerminalSession, 0, len(sessions))
		for _, s := range sessions {
			list = append(list, common.TerminalSession{ID: s.ID, Command: s.Command, Created: s.Created,
				Attached: s.Attached})
		}
		_ = ts.sendControl(common.TerminalControlMsg{Sessions: list})
	case common.TerminalOpTerminate:
		if err := d.terminals.Terminate(ctx, owner, req.Session); err != nil {
			_ = ts.refuseErr(err)
			return
		}
		_ = ts.sendControl(common.TerminalControlMsg{Session: req.Session})
	case common.TerminalOpCreate, common.TerminalOpAttach:
		if err := d.capabilities.check(terminalAppID); err != nil {
			_ = ts.refuse(common.TerminalErrDisabled, err.Error())
			return
		}
		id, attachment, err := d.openTerminal(ctx, owner, caller, req)
		if err != nil {
			_ = ts.refuseErr(err)
			return
		}
		ts.relay(id, attachment)
	default:
		_ = ts.refuse(common.TerminalErrInvalid, "unknown terminal request "+string(req.Op))
	}
}

// openTerminal creates a session for a create request and attaches to it, or attaches to
// the session an attach request names.
func (d *Deskconn) openTerminal(ctx context.Context, owner, caller string, req common.TerminalControlMsg) (
	string, *terminal.Attachment, error) {
	opts := terminal.AttachOptions{Cols: req.Cols, Rows: req.Rows, Takeover: req.Takeover, Token: req.Token,
		Since: req.Since}
	if req.Op == common.TerminalOpAttach {
		attachment, err := d.terminals.Attach(ctx, owner, req.Session, opts)
		return req.Session, attachment, err
	}

	create, err := d.terminalCreateOptions(owner, caller, req) //nolint:contextcheck // bounds its own lookups
	if err != nil {
		return "", nil, err
	}
	id, err := d.terminals.Create(ctx, create)
	if err != nil {
		return "", nil, err
	}
	attachment, err := d.terminals.Attach(ctx, owner, id, terminal.AttachOptions{Cols: req.Cols, Rows: req.Rows})
	if err != nil {
		// Nobody could ever attach to it.
		_ = d.terminals.Terminate(ctx, owner, id)
		return "", nil, err
	}
	return id, attachment, nil
}

// terminalCreateOptions is the interactive shell a create request starts, set up as
// startPtySession sets up a shell.
func (d *Deskconn) terminalCreateOptions(owner, caller string, req common.TerminalControlMsg) (
	terminal.CreateOptions, error) {
	dir := req.Dir
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := d.shellSession.resolveStartDir("")
		if err != nil {
			return terminal.CreateOptions{}, err
		}
		dir = filepath.Join(home, dir)
	}
	command, args, env := userCommand(defaultShell(), nil)
	if agentSock := d.shellSession.agentSockForAuthID(caller); agentSock != "" {
		if env == nil {
			env = os.Environ()
		}
		env = append(env, "SSH_AUTH_SOCK="+agentSock)
	}
	return terminal.CreateOptions{Owner: owner, Command: command, Args: args, Env: env, WorkDir: dir,
		Cols: req.Cols, Rows: req.Rows}, nil
}

// terminalStream is the client's side of a terminal request.
type terminalStream struct {
	stream              net.Conn
	sendKey, receiveKey []byte
	writeMu             sync.Mutex
}

func (ts *terminalStream) recv() (byte, []byte, error) {
	frame, err := common.ReadFrame(ts.stream)
	if err != nil {
		return 0, nil, err
	}
	return common.DecryptEnvelope(frame, ts.receiveKey)
}

func (ts *terminalStream) send(kind byte, plaintext []byte) error {
	envelope, err := common.BuildShellEnvelope(kind, plaintext, ts.sendKey)
	if err != nil {
		return err
	}
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return common.WriteFrame(ts.stream, envelope)
}

func (ts *terminalStream) sendControl(msg common.TerminalControlMsg) error {
	return ts.send(common.ShellMsgControl, common.MustJSON(msg))
}

func (ts *terminalStream) refuse(code, message string) error {
	return ts.sendControl(common.TerminalControlMsg{Code: code, Error: message})
}

func (ts *terminalStream) refuseErr(err error) error {
	switch {
	case errors.Is(err, terminal.ErrNotFound):
		return ts.refuse(common.TerminalErrNotFound, err.Error())
	case errors.Is(err, terminal.ErrAttached):
		return ts.refuse(common.TerminalErrAttached, err.Error())
	case errors.Is(err, terminal.ErrUnavailable):
		return ts.refuse(common.TerminalErrUnavailable, err.Error())
	case errors.Is(err, terminal.ErrLimit):
		return ts.refuse(common.TerminalErrLimit, err.Error())
	default:
		return ts.refuse(common.TerminalErrFailed, err.Error())
	}
}

// relay connects the client's stream to the session's attachment until either ends. The
// client's stream ending, however, only detaches: the session runs on.
func (ts *terminalStream) relay(id string, attachment *terminal.Attachment) {
	defer attachment.Close()
	if err := ts.sendControl(common.TerminalControlMsg{Session: id, Token: attachment.Token(),
		Offset: attachment.Offset, Snapshot: attachment.Snapshot, HistoryGap: attachment.HistoryGap}); err != nil {
		return
	}

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		// Ends the input loop below once the attachment is over.
		defer ts.stream.Close()
		for {
			event, err := attachment.Next()
			if err != nil {
				return
			}
			switch {
			case event.Exit != nil:
				_ = ts.sendControl(common.TerminalControlMsg{Op: common.TerminalOpExit, ExitCode: event.Exit})
				return
			case event.Displaced:
				_ = ts.sendControl(common.TerminalControlMsg{Op: common.TerminalOpDisplaced})
				return
			default:
				if ts.send(common.ShellMsgData, event.Output) != nil {
					return
				}
			}
		}
	}()

	detached := false
	for {
		_ = ts.stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
		kind, payload, err := ts.recv()
		if err != nil {
			break
		}
		if kind == common.ShellMsgData {
			if attachment.Write(payload) != nil {
				break
			}
			continue
		}
		var msg common.TerminalControlMsg
		if json.Unmarshal(payload, &msg) != nil {
			continue
		}
		if msg.Op == common.TerminalOpSize {
			_ = attachment.Resize(msg.Cols, msg.Rows)
		} else if msg.Op == common.TerminalOpDetach {
			detached = attachment.Detach() == nil
			break
		}
	}
	if detached {
		// The client's stream ends once the session has let go of the attachment, for it to
		// attach again right away.
		select {
		case <-outputDone:
		case <-time.After(terminalDetachTimeout):
		}
	}
	_ = attachment.Close()
	<-outputDone
	log.Debugf("terminal: attachment to session %s ended", id)
}
