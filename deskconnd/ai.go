package deskconnd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func (d *Deskconn) handleAISessionList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var path string
	if errResult = decryptArgs(enc, inv, &path); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	return encryptResult(enc, common.SummarizeAISessions(sessions))
}

func (d *Deskconn) handleAISessionPull(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	var args common.AISessionPullArgs
	if errResult = decryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(args.Path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	if args.SessionID != "" {
		var matches []common.AISessionFile
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

	byTool := make(map[string][]common.AISessionFile)
	for _, s := range sessions {
		if args.Tool != "" && s.Tool != args.Tool {
			continue
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	if len(byTool) == 0 {
		return xconn.NewInvocationError(common.ErrOperationFailed, "no local sessions found on this machine for this project")
	}

	var bundles []common.AISessionBundle
	for tool, files := range byTool {
		tarball, err := BuildAITarball(files)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		bundles = append(bundles, common.AISessionBundle{Tool: tool, Tarball: tarball})
	}

	return encryptResult(enc, bundles)
}

// aiLocalSessions discovers this machine's current Claude Code sessions for the project at
// path. Both sides of a call are expected to use the same absolute path for the same project.
func aiLocalSessions(path string) ([]common.AISessionFile, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return common.DiscoverClaudeSessions(homeDir, path)
}
