package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

type shellCommands struct {
	shell *kingpin.CmdClause

	machine      *string
	command      *[]string
	mode         *string
	forwardAgent *bool
	session      sessionFlags
}

// sessionFlags are desk shell's controls of persistent sessions.
type sessionFlags struct {
	list      *bool
	resume    *string
	takeover  *bool
	terminate *string
}

func registerShellCommands(app *kingpin.Application, cfgDirectory string) *shellCommands {
	shellCmd := app.Command("shell", "Open a shell that outlives its connection, or run a command")
	machine := machineArg(shellCmd, "ID, name or alias of machine", cfgDirectory)
	command := shellCmd.Arg("command", "Command to run instead of an interactive shell").Strings()
	mode := modeFlag(shellCmd, "Connection mode: 'routed' goes via the router over QUIC, 'direct' connects over WebRTC")
	forwardAgent := shellCmd.Flag("agent-forward",
		"Forward the local SSH agent to the remote shell (like ssh -A). Only use with hosts you trust.",
	).Short('A').Bool()
	session := sessionFlags{
		list:   shellCmd.Flag("list", "List your sessions on the machine").Bool(),
		resume: shellCmd.Flag("resume", "Attach to a running session").PlaceHolder("ID").String(),
		takeover: shellCmd.Flag("takeover",
			"With --resume, take over a session attached elsewhere, detaching it there").Bool(),
		terminate: shellCmd.Flag("terminate", "End a session and its processes").PlaceHolder("ID").String(),
	}

	return &shellCommands{
		shell:        shellCmd,
		machine:      machine,
		command:      command,
		mode:         mode,
		forwardAgent: forwardAgent,
		session:      session,
	}
}

func dispatchShellCommand(parsedCmd string, cmds *shellCommands, cfgDirectory string) bool {
	if parsedCmd != cmds.shell.FullCommand() {
		return false
	}
	exitOnErr(runShell(cfgDirectory, *cmds.machine, *cmds.command, *cmds.mode, *cmds.forwardAgent, cmds.session))
	return true
}

// runShell runs command on machine or, when command is empty, opens a persistent session
// there or controls the sessions as flags ask.
func runShell(cfgDirectory, machine string, command []string, mode string, forwardAgent bool,
	flags sessionFlags) error {
	if err := flags.check(command, forwardAgent); err != nil {
		return err
	}
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}
	mode = deskconn.DefaultMode(cfgDirectory, realm, mode)
	switch {
	case *flags.list:
		return listSessions(mode, realm, cfgDirectory, machine)
	case *flags.terminate != "":
		return terminateSession(mode, realm, cfgDirectory, machine, *flags.terminate)
	}

	var agentSock string
	if forwardAgent {
		agentSock = os.Getenv("SSH_AUTH_SOCK")
		if agentSock == "" {
			fmt.Fprintln(os.Stderr, "warning: SSH_AUTH_SOCK not set, continuing without agent forwarding")
		}
	}
	defer setupAgentForward(mode, realm, cfgDirectory, agentSock)()

	if len(command) > 0 {
		return deskconn.RunExec(context.Background(), mode, realm, cfgDirectory, "", command)
	}
	return attachSession(mode, realm, cfgDirectory, machine, *flags.resume, *flags.takeover)
}

// agentForwardReadyTimeout bounds how long "desk shell -A" waits for the remote agent
// forwarding listener to come up before giving up and starting a normal (unforwarded) shell.
const agentForwardReadyTimeout = 5 * time.Second

// setupAgentForward starts SSH-agent forwarding (see deskconn.RunAgentForward) when agentSock
// is non-empty, opening its own raw stream/channel independent of the shell's. It blocks until
// the remote listener is confirmed ready (or setup fails/times out) so the shell can safely
// start right after. Forwarding failures are reported as warnings and never block the shell
// from starting. The returned func stops forwarding and must be called (deferred) once the
// shell exits.
func setupAgentForward(mode, realm, cfgDirectory, agentSock string) func() {
	if agentSock == "" {
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	common.SafeGo(func() {
		_ = deskconn.RunAgentForward(ctx, mode, realm, cfgDirectory, agentSock, ready)
	})

	select {
	case err := <-ready:
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: agent forwarding unavailable: %v\n", err)
		}
	case <-time.After(agentForwardReadyTimeout):
		fmt.Fprintln(os.Stderr, "warning: agent forwarding setup timed out, continuing without it")
	}
	return cancel
}
