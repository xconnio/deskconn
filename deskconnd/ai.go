package deskconnd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/ai"
	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func aiDecryptArgs(enc *encryptionKeys, inv *xconn.Invocation, out any) *xconn.InvocationResult {
	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	return nil
}

func aiEncryptResult(enc *encryptionKeys, result any) *xconn.InvocationResult {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleAISessionList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	var path string
	if errResult := aiDecryptArgs(enc, inv, &path); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	var summaries []common.AISessionSummary
	for _, s := range sessions {
		summaries = append(summaries, common.AISessionSummary{
			Tool:      s.Tool,
			SessionID: strings.TrimSuffix(filepath.Base(s.Path), ".jsonl"),
			Title:     ai.SessionTitle(s.Path),
			UpdatedAt: s.ModTime,
			Size:      s.Size,
		})
	}

	return aiEncryptResult(enc, summaries)
}

func (d *Deskconn) handleAISessionPull(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	var args common.AISessionPullArgs
	if errResult := aiDecryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(args.Path)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	if args.SessionID != "" {
		var matches []ai.SessionFile
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

	byTool := make(map[string][]ai.SessionFile)
	for _, s := range sessions {
		if args.Tool != "" && s.Tool != args.Tool {
			continue
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	if len(byTool) == 0 {
		return xconn.NewInvocationError(common.ErrOperationFailed, "no local sessions found on this device for this project")
	}

	var bundles []common.AISessionBundle
	for tool, files := range byTool {
		tarball, err := ai.BuildTarball(files)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		bundles = append(bundles, common.AISessionBundle{Tool: tool, Tarball: tarball})
	}

	return aiEncryptResult(enc, bundles)
}

// aiLocalSessions discovers this device's current Claude Code sessions for the project at
// path. Both sides of a call are expected to use the same absolute path for the same project.
func aiLocalSessions(path string) ([]ai.SessionFile, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}
	return ai.DiscoverClaudeSessions(homeDir, path)
}
