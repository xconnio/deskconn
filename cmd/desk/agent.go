package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	resumeMode      *string
}

func registerAgentCommands(app *kingpin.Application, cfgDirectory string) *agentCommands {
	agentCmd := app.Command("agent", "Pull and resume Claude Code CLI sessions directly with another device")

	lsCmd := agentCmd.Command("ls", "List Claude Code sessions for this project, on this device or another one")
	// Unlike deviceArg, machine is optional here: omitting it lists this device's own sessions.
	lsMachine := new(string)
	if !standaloneRequested() {
		lsMachine = lsCmd.Arg("machine", "Device to list sessions on (default: this device)").
			HintAction(deviceCompletions(cfgDirectory)).String()
	}
	lsMode := modeFlag(lsCmd,
		"Connection mode: 'quic' uses QUIC stream via router, default uses daemon persistent session")

	pullCmd := agentCmd.Command("pull", "Pull claude sessions from another device onto this one")
	pullMachine := deviceArg(pullCmd, "machine", "Device to pull sessions from", cfgDirectory)
	pullSessionID := pullCmd.Arg("session-id",
		"Only pull the session matching this id (or a unique prefix of one); default pulls all").String()
	pullMode := modeFlag(pullCmd,
		"Connection mode: 'quic' uses QUIC stream via router, default uses daemon persistent session")

	resumeCmd := agentCmd.Command("resume", "Resume a Claude Code session by id (see `agent ls`)")
	resumeSessionID := resumeCmd.Arg("session-id", "Session id (or a unique prefix of one) to resume").Required().String()
	resumePrintOnly := resumeCmd.Flag("print-only",
		"Only print the resume command; don't launch").Bool()
	resumeRemote := resumeCmd.Flag("remote",
		"Run claude directly on this device instead of resuming a locally pulled session").
		HintAction(deviceCompletions(cfgDirectory)).String()
	resumeMode := modeFlag(resumeCmd,
		"Connection mode when --remote is set: 'quic' uses QUIC stream via router, default uses daemon persistent session")

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
			err := runAgentResumeRemote(cfgDirectory, *cmds.resumeRemote, *cmds.resumeSessionID, *cmds.resumeMode)
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

func runAgentLs(cfgDirectory, machine, mode string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}

	var sessions []common.AISessionSummary
	switch {
	case machine == "" && !standaloneRequested():
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		local, err := common.DiscoverClaudeSessions(homeDir, path)
		if err != nil {
			return fmt.Errorf("failed to discover local sessions: %w", err)
		}
		sessions = common.SummarizeAISessions(local)
	case mode == ModeQUIC:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		quicSess, err := common.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		sessions, err = deskconn.CallAISessionList(quicSess.Session, path)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	case mode == ModeP2P:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Leave() }()
		sessions, err = deskconn.CallAISessionList(p2pSess, path)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	default:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(),
			common.UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock")), common.LocalRealm)
		if err != nil {
			return fmt.Errorf("could not reach local daemon: %w", err)
		}
		defer func() { _ = localSession.Leave() }()
		sessions, err = deskconn.CallAISessionListProxy(localSession, realm, path)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	}
	if len(sessions) == 0 {
		if machine == "" && !standaloneRequested() {
			fmt.Println("no claude sessions found on this device for this project")
			return nil
		}
		fmt.Printf("no claude sessions found on %s for this project\n", machine)
		return nil
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.Header([]string{"SESSION-ID", "TITLE", "UPDATED"})
	for _, s := range sessions {
		_ = table.Append([]any{s.SessionID, s.Title, formatSince(time.Since(s.UpdatedAt))})
	}
	return table.Render()
}

func runAgentPull(cfgDirectory, machine, mode, sessionID string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}

	var bundles []common.AISessionBundle
	switch mode {
	case ModeQUIC:
		var realm string
		realm, err = deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		quicSess, qErr := common.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if qErr != nil {
			return qErr
		}
		defer quicSess.Connection().Close()
		bundles, err = deskconn.CallAISessionPull(quicSess.Session, path, "", sessionID)
	case ModeP2P:
		var realm string
		realm, err = deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		p2pSess, pErr := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
		if pErr != nil {
			return pErr
		}
		defer func() { _ = p2pSess.Leave() }()
		bundles, err = deskconn.CallAISessionPull(p2pSess, path, "", sessionID)
	default:
		var realm string
		realm, err = deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		localSession, lErr := xconn.ConnectAnonymous(context.Background(),
			common.UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock")), common.LocalRealm)
		if lErr != nil {
			return fmt.Errorf("could not reach local daemon: %w", lErr)
		}
		defer func() { _ = localSession.Leave() }()
		bundles, err = deskconn.CallAISessionPullProxy(localSession, realm, path, "", sessionID)
	}
	if err != nil {
		return fmt.Errorf("failed to pull sessions from %s: %w", machine, err)
	}
	if len(bundles) == 0 {
		return fmt.Errorf("no claude sessions found on %s for this project", machine)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	for _, bundle := range bundles {
		fileCount, err := deskconn.ExtractAITarball(bundle.Tarball, homeDir, path)
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

	sessions, err := common.DiscoverClaudeSessions(homeDir, path)
	if err != nil {
		return fmt.Errorf("failed to discover local sessions: %w", err)
	}
	if len(sessions) == 0 {
		return errors.New("no local claude sessions found for this project; run `desk agent pull <machine>` first")
	}

	var matches []common.AISessionFile
	for _, s := range sessions {
		id := strings.TrimSuffix(filepath.Base(s.Path), ".jsonl")
		if strings.HasPrefix(id, sessionID) {
			matches = append(matches, s)
		}
	}
	switch {
	case len(matches) == 0:
		return fmt.Errorf("no local session matching %q; run `desk agent ls` to see available ids", sessionID)
	case len(matches) > 1:
		return fmt.Errorf("%q matches more than one local session; use a longer prefix", sessionID)
	}

	match := matches[0]
	tool := match.Tool
	fullID := strings.TrimSuffix(filepath.Base(match.Path), ".jsonl")

	cmdName, args := agentResumeCommand(fullID)
	if printOnly {
		fmt.Println(strings.Join(append([]string{cmdName}, args...), " "))
		return nil
	}

	return launch(tool, cmdName, args)
}

func agentResumeCommand(sessionID string) (string, []string) {
	args := []string{"--resume"}
	if sessionID != "" {
		args = append(args, sessionID)
	}
	return "claude", args
}

func runAgentResumeRemote(cfgDirectory, machine, sessionID, mode string) error {
	path, err := agentProjectPath()
	if err != nil {
		return err
	}
	realm, err := deviceRealm(machine, cfgDirectory)
	if err != nil {
		return fmt.Errorf("unknown device %q: %w", machine, err)
	}

	cmdName, args := agentResumeCommand(sessionID)
	fullArgs := append([]string{
		"bash", "-c", `cd -- "$HOME/$1" && shift && exec "$@"`, "bash", path, cmdName,
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
