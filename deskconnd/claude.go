package deskconnd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// claudeInstalled reports whether Claude Code is installed: whether a login shell, as the
// web app's terminals use to run it, finds claude. Checked once, when deskconnd starts.
// ponytail: installing or removing Claude Code takes a deskconnd restart to show.
var claudeInstalled = func() bool { //nolint:gochecknoglobals // tests replace it
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "bash", "-lc", "command -v claude").Run() == nil
}

func (d *Deskconn) handleClaudeSessionList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args common.ClaudeSessionListArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}
	if args.Offset < 0 || args.Limit < 0 {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "offset and limit can't be negative")
	}

	sessions, err := claudeLocalSessions(args.Path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	// Only the page's sessions are summarized: that reads each one's file.
	sessions = sessions[min(args.Offset, len(sessions)):]
	if args.Limit > 0 {
		sessions = sessions[:min(args.Limit, len(sessions))]
	}
	return encryptResult(enc, common.SummarizeClaudeSessions(sessions))
}

func (d *Deskconn) handleClaudeSessionPull(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args common.ClaudeSessionPullArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	sessions, err := claudeLocalSessions(args.Path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	if args.SessionID != "" {
		var matches []common.ClaudeSessionFile
		for _, s := range sessions {
			id := strings.TrimSuffix(filepath.Base(s.Path), ".jsonl")
			if strings.HasPrefix(id, args.SessionID) {
				matches = append(matches, s)
			}
		}
		switch {
		case len(matches) == 0:
			return xconn.NewInvocationError(common.ErrOperationFailed, fmt.Sprintf("no session matching %q", args.SessionID))
		case len(matches) > 1:
			return xconn.NewInvocationError(common.ErrOperationFailed,
				fmt.Sprintf("%q matches more than one session; use a longer prefix", args.SessionID))
		}
		sessions = matches
	}

	byTool := make(map[string][]common.ClaudeSessionFile)
	for _, s := range sessions {
		if args.Tool != "" && s.Tool != args.Tool {
			continue
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	if len(byTool) == 0 {
		return xconn.NewInvocationError(common.ErrOperationFailed, "no local sessions found on this machine for this project")
	}

	var bundles []common.ClaudeSessionBundle
	for tool, files := range byTool {
		tarball, err := BuildClaudeTarball(files)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		bundles = append(bundles, common.ClaudeSessionBundle{Tool: tool, Tarball: tarball})
	}

	return encryptResult(enc, bundles)
}

func (d *Deskconn) handleClaudeProjectList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	projects, err := common.DiscoverClaudeProjects(homeDir)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, projects)
}

// claudeLocalSessions discovers this machine's current Claude Code sessions for the project at
// path: relative to home, as `desk agent` sends it to match the project across machines, or
// absolute, as the web app sends a directory from ProcedureClaudeProjectList.
func claudeLocalSessions(path string) ([]common.ClaudeSessionFile, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return common.DiscoverClaudeSessions(homeDir, path)
}
