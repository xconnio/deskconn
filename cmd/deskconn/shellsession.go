package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

// check rejects session flags that don't go together, or with a command or -A.
func (f sessionFlags) check(command []string, forwardAgent bool) error {
	controls := 0
	for _, set := range []bool{*f.list, *f.resume != "", *f.terminate != ""} {
		if set {
			controls++
		}
	}
	switch {
	case controls == 0 && !*f.takeover:
		return nil
	case len(command) > 0:
		return &usageError{"--list, --resume, --takeover and --terminate can't be used with a command"}
	case controls > 1:
		return &usageError{"use only one of --list, --resume and --terminate"}
	case *f.takeover && *f.resume == "":
		return &usageError{"--takeover goes with --resume"}
	case forwardAgent:
		return &usageError{"-A forwards the agent to a new session only"}
	}
	return nil
}

// attachSession attaches to session on machine, or creates one if session is empty. Input
// that isn't a terminal, or a machine without persistent sessions, gets a shell that ends
// with its connection instead.
func attachSession(mode, realm, cfgDirectory, machine, session string, takeover bool) error {
	req := common.TerminalControlMsg{Op: common.TerminalOpCreate}
	if session != "" {
		req = common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: session, Takeover: takeover}
	}
	if req.Op == common.TerminalOpCreate && !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		// Input from a pipe or a file: a shell that ends with it, as it always was.
		return deskconn.RunShell(context.Background(), mode, realm, cfgDirectory)
	}
	end, session, err := deskconn.RunTerminal(context.Background(), mode, realm, cfgDirectory, req)
	if errors.Is(err, deskconn.ErrTerminalUnsupported) && req.Op == common.TerminalOpCreate {
		fmt.Fprintf(os.Stderr, "%s has no persistent sessions: this shell ends when it disconnects\n", machine)
		return deskconn.RunShell(context.Background(), mode, realm, cfgDirectory)
	}
	if err != nil {
		return sessionError(err, machine, req.Session)
	}
	resume := fmt.Sprintf("desk shell %s --resume %s", machine, session)
	switch end {
	case deskconn.TerminalDetached:
		fmt.Fprintf(os.Stderr, "\nDetached from session %s. It runs on: %s\n", session, resume)
	case deskconn.TerminalDisplaced:
		fmt.Fprintf(os.Stderr, "\nSession %s was taken over elsewhere. It runs on: %s --takeover\n",
			session, resume)
		return errAlreadyReported
	}
	return nil
}

// sessionError explains the machine refusing a request about session.
func sessionError(err error, machine, session string) error {
	var refused *deskconn.TerminalError
	switch {
	case errors.Is(err, deskconn.ErrTerminalUnsupported):
		return fmt.Errorf("%s has no persistent sessions: its DeskConn needs updating, or its terminal is disabled",
			machine)
	case !errors.As(err, &refused):
		return err
	}
	switch refused.Code {
	case common.TerminalErrNotFound:
		return fmt.Errorf("no session %s on %s: it has ended, or isn't yours (desk shell %s --list)",
			session, machine, machine)
	case common.TerminalErrAttached:
		return fmt.Errorf("session %s is attached elsewhere: add --takeover to take it over", session)
	case common.TerminalErrLimit:
		return fmt.Errorf("%s runs as many sessions as it allows: end one with desk shell %s --terminate ID (%w)",
			machine, machine, err)
	case common.TerminalErrDisabled:
		return fmt.Errorf("the terminal is disabled on %s", machine)
	}
	return err
}

func listSessions(mode, realm, cfgDirectory, machine string) error {
	reply, err := deskconn.TerminalRequest(context.Background(), mode, realm, cfgDirectory,
		common.TerminalControlMsg{Op: common.TerminalOpList})
	if err != nil {
		return sessionError(err, machine, "")
	}
	if len(reply.Sessions) == 0 {
		fmt.Printf("No sessions on %s\n", machine)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTARTED\tATTACHED\tCOMMAND")
	for _, s := range reply.Sessions {
		attached := "no"
		if s.Attached {
			attached = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.ID, s.Created.Local().Format(time.DateTime), attached, s.Command)
	}
	return w.Flush()
}

func terminateSession(mode, realm, cfgDirectory, machine, session string) error {
	_, err := deskconn.TerminalRequest(context.Background(), mode, realm, cfgDirectory,
		common.TerminalControlMsg{Op: common.TerminalOpTerminate, Session: session})
	if err != nil {
		return sessionError(err, machine, session)
	}
	fmt.Printf("Terminated session %s\n", session)
	return nil
}
