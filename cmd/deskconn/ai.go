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

	"github.com/xconnio/deskconn"
	"github.com/xconnio/deskconn/ai"
	"github.com/xconnio/xconn-go"
)

type aiCommands struct {
	ls     *kingpin.CmdClause
	sync   *kingpin.CmdClause
	resume *kingpin.CmdClause

	lsMachine *string
	lsMode    *string

	syncMachine   *string
	syncMode      *string
	syncSessionID *string

	resumeSessionID *string
	resumePrintOnly *bool
	resumeRemote    *string
	resumeMode      *string
}

func registerAICommands(app *kingpin.Application, cfgDirectory string) *aiCommands {
	aiCmd := app.Command("ai", "Sync and resume Claude Code CLI sessions directly with another device")

	lsCmd := aiCmd.Command("ls", "List Claude Code sessions available on another device")
	lsMachine := lsCmd.Arg("machine", "Device to list sessions on").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	lsMode := lsCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC, 'routed' uses WAMP RPC",
	).Enum(ModeQUIC, ModeP2P, ModeRouted)

	syncCmd := aiCmd.Command("sync", "Pull claude sessions from another device onto this one")
	syncMachine := syncCmd.Arg("machine", "Device to pull sessions from").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	syncSessionID := syncCmd.Arg("session-id",
		"Only pull the session matching this id (or a unique prefix of one); default pulls all").String()
	syncMode := syncCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC, 'routed' uses WAMP RPC",
	).Enum(ModeQUIC, ModeP2P, ModeRouted)

	resumeCmd := aiCmd.Command("resume", "Resume a Claude Code session by id (see `ai ls`)")
	resumeSessionID := resumeCmd.Arg("session-id", "Session id (or a unique prefix of one) to resume").Required().String()
	resumePrintOnly := resumeCmd.Flag("print-only",
		"Only print the resume command; don't launch").Bool()
	resumeRemote := resumeCmd.Flag("remote",
		"Run claude directly on this device instead of resuming a locally synced session").
		HintAction(deviceCompletions(cfgDirectory)).String()
	resumeMode := resumeCmd.Flag("mode",
		"Connection mode when --remote is set: 'quic' uses QUIC stream via router, 'routed' connects directly "+
			"through the cloud router, default proxies through the local daemon over WebRTC",
	).Enum(ModeQUIC, ModeP2P, ModeRouted)

	return &aiCommands{
		ls:              lsCmd,
		sync:            syncCmd,
		resume:          resumeCmd,
		lsMachine:       lsMachine,
		lsMode:          lsMode,
		syncMachine:     syncMachine,
		syncMode:        syncMode,
		syncSessionID:   syncSessionID,
		resumeSessionID: resumeSessionID,
		resumePrintOnly: resumePrintOnly,
		resumeRemote:    resumeRemote,
		resumeMode:      resumeMode,
	}
}

func dispatchAICommand(parsedCmd string, cmds *aiCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand():
		if err := runAILs(cfgDirectory, *cmds.lsMachine, *cmds.lsMode); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	case cmds.sync.FullCommand():
		if err := runAISync(cfgDirectory, *cmds.syncMachine, *cmds.syncMode, *cmds.syncSessionID); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	case cmds.resume.FullCommand():
		if *cmds.resumeRemote != "" {
			if err := runAIResumeRemote(cfgDirectory, *cmds.resumeRemote, *cmds.resumeSessionID, *cmds.resumeMode); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		} else if err := runAIResume(*cmds.resumeSessionID, *cmds.resumePrintOnly); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	default:
		return false
	}
	return true
}

// aiProjectPath returns the current directory's path relative to $HOME - not the absolute
// path - since that's what identifies "the same project" across your machines regardless of
// username or where each machine's home directory happens to live.
func aiProjectPath() (string, error) {
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

func runAILs(cfgDirectory, machine, mode string) error {
	path, err := aiProjectPath()
	if err != nil {
		return err
	}

	var sessions []deskconn.AISessionSummary
	switch mode {
	case ModeQUIC:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		sessions, err = deskconn.CallAISessionList(quicSess.Session, path)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	case ModeRouted:
		session, err := ConnectToMachine(context.Background(), machine, cfgDirectory, false)
		if err != nil {
			return err
		}
		defer func() { _ = session.Leave() }()
		sessions, err = deskconn.CallAISessionList(session, path)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	default:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(),
			fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory), deskconn.LocalRealm)
		if err != nil {
			return fmt.Errorf("could not reach local daemon: %w", err)
		}
		defer func() { _ = localSession.Leave() }()
		sessions, err = deskconn.CallAISessionListProxy(localSession, realm, path, mode == ModeP2P)
		if err != nil {
			return fmt.Errorf("failed to list sessions on %s: %w", machine, err)
		}
	}
	if len(sessions) == 0 {
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

func runAISync(cfgDirectory, machine, mode, sessionID string) error {
	path, err := aiProjectPath()
	if err != nil {
		return err
	}

	var bundles []deskconn.AISessionBundle
	switch mode {
	case ModeQUIC:
		var realm string
		realm, err = deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		quicSess, qErr := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if qErr != nil {
			return qErr
		}
		defer quicSess.Connection().Close()
		bundles, err = deskconn.CallAISessionPull(quicSess.Session, path, "", sessionID)
	case ModeRouted:
		session, sErr := ConnectToMachine(context.Background(), machine, cfgDirectory, false)
		if sErr != nil {
			return sErr
		}
		defer func() { _ = session.Leave() }()
		bundles, err = deskconn.CallAISessionPull(session, path, "", sessionID)
	default:
		var realm string
		realm, err = deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		localSession, lErr := xconn.ConnectAnonymous(context.Background(),
			fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory), deskconn.LocalRealm)
		if lErr != nil {
			return fmt.Errorf("could not reach local daemon: %w", lErr)
		}
		defer func() { _ = localSession.Leave() }()
		bundles, err = deskconn.CallAISessionPullProxy(localSession, realm, path, "", sessionID, mode == ModeP2P)
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
		fileCount, err := ai.ExtractTarball(bundle.Tarball, homeDir, path)
		if err != nil {
			return fmt.Errorf("failed to restore %s session: %w", bundle.Tool, err)
		}
		fmt.Printf("pulled %s session (%d file(s)) from %s\n", bundle.Tool, fileCount, machine)
	}
	return nil
}

func runAIResume(sessionID string, printOnly bool) error {
	path, err := aiProjectPath()
	if err != nil {
		return err
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sessions, err := ai.DiscoverClaudeSessions(homeDir, path)
	if err != nil {
		return fmt.Errorf("failed to discover local sessions: %w", err)
	}
	if len(sessions) == 0 {
		return errors.New("no local claude sessions found for this project; run `desk ai sync <machine>` first")
	}

	var matches []ai.SessionFile
	for _, s := range sessions {
		id := strings.TrimSuffix(filepath.Base(s.Path), ".jsonl")
		if strings.HasPrefix(id, sessionID) {
			matches = append(matches, s)
		}
	}
	switch {
	case len(matches) == 0:
		return fmt.Errorf("no local session matching %q; run `desk ai ls <machine>` to see available ids", sessionID)
	case len(matches) > 1:
		return fmt.Errorf("%q matches more than one local session; use a longer prefix", sessionID)
	}

	match := matches[0]
	tool := match.Tool
	fullID := strings.TrimSuffix(filepath.Base(match.Path), ".jsonl")

	cmdName, args := aiResumeCommand(fullID)
	if printOnly {
		fmt.Println(strings.Join(append([]string{cmdName}, args...), " "))
		return nil
	}

	return launch(tool, cmdName, args)
}

func aiResumeCommand(sessionID string) (string, []string) {
	args := []string{"--resume"}
	if sessionID != "" {
		args = append(args, sessionID)
	}
	return "claude", args
}

func runAIResumeRemote(cfgDirectory, machine, sessionID, mode string) error {
	path, err := aiProjectPath()
	if err != nil {
		return err
	}

	cmdName, args := aiResumeCommand(sessionID)
	fullArgs := append([]string{
		"bash", "-c", `cd -- "$HOME/$1" && shift && exec "$@"`, "bash", path, cmdName,
	}, args...)

	switch mode {
	case ModeQUIC:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		return deskconn.StartInteractiveCommand(quicSess.Session, "", deskconn.ProcedureExec, fullArgs...)
	case ModeRouted:
		session, err := ConnectToMachine(context.Background(), machine, cfgDirectory, false)
		if err != nil {
			return err
		}
		defer func() { _ = session.Leave() }()
		return deskconn.StartInteractiveCommand(session, "", deskconn.ProcedureExec, fullArgs...)
	default:
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return fmt.Errorf("unknown device %q: %w", machine, err)
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(),
			fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory), deskconn.LocalRealm)
		if err != nil {
			return fmt.Errorf("could not reach local daemon: %w", err)
		}
		defer func() { _ = localSession.Leave() }()
		return deskconn.StartInteractiveCommand(localSession, realm, deskconn.ProcedureProxyExec, fullArgs...)
	}
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
