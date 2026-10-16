package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/xconn-go"
)

type agentCommands struct {
	ls     *kingpin.CmdClause
	pull   *kingpin.CmdClause
	resume *kingpin.CmdClause

	lsMachine *string
	lsMode    *string

	pullMachine   *string
	pullMode      *string
	pullSessionID *string

	resumeSessionID *string
	resumePrintOnly *bool
	resumeRemote    *string
	resumeAgent     *string
	resumeMode      *string
}

func registerAgentCommands(app *kingpin.Application, cfgDirectory string) *agentCommands {
	const modeHelp = "Connection mode: 'routed' goes via the router over QUIC, 'direct' connects over WebRTC, " +
		"default uses the daemon's persistent session"
	agentCmd := app.Command("agent", "List, pull, and resume Claude Code and Codex sessions")

	lsCmd := agentCmd.Command("ls", "List agent sessions for this project, on this machine or another one")
	// Unlike machineArg, machine is optional here: omitting it lists this machine's own sessions.
	lsMachine := new(string)
	if !standaloneRequested() {
		lsMachine = lsCmd.Arg("machine", "Machine to list sessions on (default: this machine)").
			HintAction(machineCompletions(cfgDirectory)).String()
	}
	lsMode := modeFlag(lsCmd, modeHelp)

	pullCmd := agentCmd.Command("pull", "Pull agent sessions from another machine onto this one")
	pullMachine := machineArg(pullCmd, "Machine to pull sessions from", cfgDirectory)
	pullSessionID := pullCmd.Arg("session-id",
		"Only pull the session matching this id (or a unique prefix of one); default pulls all").String()
	pullMode := modeFlag(pullCmd, modeHelp)

	resumeCmd := agentCmd.Command("resume", "Resume an agent session by id (see `agent ls`)")
	resumeSessionID := resumeCmd.Arg("session-id", "Session id (or a unique prefix of one) to resume").Required().String()
	resumePrintOnly := resumeCmd.Flag("print-only",
		"Only print the resume command; don't launch").Bool()
	resumeRemote := resumeCmd.Flag("remote",
		"Run the agent directly on this machine instead of resuming a locally pulled session").
		HintAction(machineCompletions(cfgDirectory)).String()
	resumeAgent := resumeCmd.Flag("agent",
		"The session's agent when --remote is set (see `agent ls <machine>`)").
		Default(common.ClaudeTool).Enum(common.AgentTools()...)
	resumeMode := modeFlag(resumeCmd, modeHelp+" (with --remote)")

	return &agentCommands{
		ls:              lsCmd,
		pull:            pullCmd,
		resume:          resumeCmd,
		lsMachine:       lsMachine,
		lsMode:          lsMode,
		pullMachine:     pullMachine,
		pullMode:        pullMode,
		pullSessionID:   pullSessionID,
		resumeSessionID: resumeSessionID,
		resumePrintOnly: resumePrintOnly,
		resumeRemote:    resumeRemote,
		resumeAgent:     resumeAgent,
		resumeMode:      resumeMode,
	}
}

func dispatchAgentCommand(parsedCmd string, cmds *agentCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand():
		if err := runAgentLs(cfgDirectory, *cmds.lsMachine, *cmds.lsMode); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	case cmds.pull.FullCommand():
		if err := runAgentPull(cfgDirectory, *cmds.pullMachine, *cmds.pullMode, *cmds.pullSessionID); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	case cmds.resume.FullCommand():
		if *cmds.resumeRemote != "" {
			err := runAgentResumeRemote(cfgDirectory, *cmds.resumeRemote, *cmds.resumeAgent, *cmds.resumeSessionID,
				*cmds.resumeMode)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		} else if err := runAgentResume(*cmds.resumeSessionID, *cmds.resumePrintOnly); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	default:
		return false
	}
	return true
}

// agentProjectPath returns the current directory's path relative to $HOME - not the absolute
// path - since that's what identifies "the same project" across your machines regardless of
// username or where each machine's home directory happens to live.
func agentProjectPath() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	path, err := filepath.Rel(homeDir, cwd)
	if err != nil {
		return "", fmt.Errorf("failed to resolve current directory relative to home: %w", err)
	}
	return path, nil
}

// agentMachine calls a machine's agent procedures: each agent's are only there while it's
// installed on the machine, and its app enabled.
type agentMachine struct {
	list  func(tool, path string) ([]common.AgentSessionSummary, error)
	pull  func(tool, path, sessionID string) ([]common.AgentSessionBundle, error)
	close func()
}

// connectAgentMachine connects to machine with mode: 'routed' or 'direct' itself, otherwise
// through the daemon's persistent session.
func connectAgentMachine(cfgDirectory, machine, mode string) (*agentMachine, error) {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return nil, fmt.Errorf("unknown machine %q: %w", machine, err)
	}

	var session *xconn.Session
	var closeFn func()
	switch mode {
	case ModeRouted:
		quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		session, closeFn = quicSess.Session, func() { _ = quicSess.Connection().Close() }
	case ModeDirect:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		session, closeFn = p2pSess, func() { _ = p2pSess.Leave() }
	default:
		localSession, err := xconn.ConnectAnonymous(context.Background(),
			common.UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock")), common.LocalRealm)
		if err != nil {
			return nil, fmt.Errorf("could not reach local daemon: %w", err)
		}
		return &agentMachine{
			list: func(tool, path string) ([]common.AgentSessionSummary, error) {
				return deskconn.CallAgentSessionListProxy(localSession, realm, tool, path)
			},
			pull: func(tool, path, sessionID string) ([]common.AgentSessionBundle, error) {
				return deskconn.CallAgentSessionPullProxy(localSession, realm, tool, path, sessionID)
			},
			close: func() { _ = localSession.Leave() },
		}, nil
	}
	return &agentMachine{
		list: func(tool, path string) ([]common.AgentSessionSummary, error) {
			return deskconn.CallAgentSessionList(session, tool, path)
		},
		pull: func(tool, path, sessionID string) ([]common.AgentSessionBundle, error) {
			return deskconn.CallAgentSessionPull(session, tool, path, sessionID)
		},
		close: closeFn,
	}, nil
}

// agentMissing reports whether err is a machine not having an agent's procedures: the agent
// isn't installed there, or its app is disabled.
func agentMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), common.ErrNoSuchProcedure)
}

func runAgentLs(cfgDirectory, machine, mode string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}

	where := "this machine"
	var sessions []common.AgentSessionSummary
	if machine == "" && !standaloneRequested() {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		for _, tool := range common.AgentTools() {
			local, err := common.DiscoverAgentSessions(tool, homeDir, path)
			if err != nil {
				return fmt.Errorf("failed to discover local %s sessions: %w", tool, err)
			}
			sessions = append(sessions, common.SummarizeAgentSessions(local)...)
		}
	} else {
		where = machine
		m, err := connectAgentMachine(cfgDirectory, machine, mode)
		if err != nil {
			return err
		}
		defer m.close()
		for _, tool := range common.AgentTools() {
			found, err := m.list(tool, path)
			if agentMissing(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to list %s sessions on %s: %w", tool, machine, err)
			}
			sessions = append(sessions, found...)
		}
	}
	if len(sessions) == 0 {
		fmt.Printf("no agent sessions found on %s for this project\n", where)
		return nil
	}
	slices.SortFunc(sessions, func(a, b common.AgentSessionSummary) int { return b.UpdatedAt.Compare(a.UpdatedAt) })

	table := tablewriter.NewWriter(os.Stdout)
	table.Header([]string{"AGENT", "SESSION-ID", "TITLE", "UPDATED"})
	for _, s := range sessions {
		_ = table.Append([]any{s.Tool, s.SessionID, s.Title, formatSince(time.Since(s.UpdatedAt))})
	}
	return table.Render()
}

func runAgentPull(cfgDirectory, machine, mode, sessionID string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}

	m, err := connectAgentMachine(cfgDirectory, machine, mode)
	if err != nil {
		return err
	}
	defer m.close()

	// Each agent errors when it has nothing to pull: that's only an error if none has.
	var bundles []common.AgentSessionBundle
	var pullErr error
	for _, tool := range common.AgentTools() {
		found, err := m.pull(tool, path, sessionID)
		if agentMissing(err) {
			continue
		}
		if err != nil {
			pullErr = cmp.Or(pullErr, fmt.Errorf("failed to pull %s sessions from %s: %w", tool, machine, err))
			continue
		}
		bundles = append(bundles, found...)
	}
	if len(bundles) == 0 {
		return cmp.Or(pullErr, fmt.Errorf("no agent sessions found on %s for this project", machine))
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	for _, bundle := range bundles {
		fileCount, err := deskconn.ExtractAgentTarball(bundle.Tool, bundle.Tarball, homeDir, path)
		if err != nil {
			return fmt.Errorf("failed to restore %s session: %w", bundle.Tool, err)
		}
		fmt.Printf("pulled %s session (%d file(s)) from %s\n", bundle.Tool, fileCount, machine)
	}
	return nil
}

func runAgentResume(sessionID string, printOnly bool) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	var matches []common.AgentSessionFile
	for _, tool := range common.AgentTools() {
		sessions, err := common.DiscoverAgentSessions(tool, homeDir, path)
		if err != nil {
			return fmt.Errorf("failed to discover local %s sessions: %w", tool, err)
		}
		for _, s := range sessions {
			if strings.HasPrefix(s.ID, sessionID) {
				matches = append(matches, s)
			}
		}
	}
	switch {
	case len(matches) == 0:
		return fmt.Errorf("no local session matching %q; run `desk agent ls` to see available ids, "+
			"or `desk agent pull <machine>` first", sessionID)
	case len(matches) > 1:
		return fmt.Errorf("%q matches more than one local session; use a longer prefix", sessionID)
	}

	match := matches[0]
	cmdName, args := agentResumeCommand(match.Tool, match.ID)
	if printOnly {
		fmt.Println(strings.Join(append([]string{cmdName}, args...), " "))
		return nil
	}

	return launch(match.Tool, cmdName, args)
}

// agentResumeCommand returns the command resuming tool's session sessionID: its picker if
// sessionID is empty.
func agentResumeCommand(tool, sessionID string) (string, []string) {
	args := []string{"--resume"}
	if tool == common.CodexTool {
		args = []string{"resume"}
	}
	if sessionID != "" {
		args = append(args, sessionID)
	}
	return tool, args
}

func runAgentResumeRemote(cfgDirectory, machine, tool, sessionID, mode string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return fmt.Errorf("unknown machine %q: %w", machine, err)
	}

	// An interactive login shell, like the web app's terminals, so the agent is found on the
	// user's PATH, nvm's included (see userd's agentInstalled).
	cmdName, args := agentResumeCommand(tool, sessionID)
	fullArgs := append([]string{
		"bash", "-lic", `cd -- "$HOME/$1" && shift && exec "$@"`, "bash", path, cmdName,
	}, args...)

	return deskconn.RunExec(context.Background(), mode, realm, cfgDirectory, fullArgs)
}

// launch runs tool as a foreground child, inheriting stdio.
func launch(tool, cmdName string, args []string) error {
	cmd := exec.Command(cmdName, args...) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch %s: %w", cmdName, err)
	}

	runErr := cmd.Wait()

	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		fmt.Fprintf(os.Stderr, "warning: %s exited abnormally: %v\n", tool, runErr)
	}
	return nil
}
