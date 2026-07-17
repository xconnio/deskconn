package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/deskconn/ai"
	"github.com/xconnio/xconn-go"
)

const (
	ProcedureAISessionList = "io.xconn.deskconn.deskconnd.ai.session.list"

	ProcedureAISessionPull = "io.xconn.deskconn.deskconnd.ai.session.pull"
)

// AISessionSummary describes one session file found on a device.
type AISessionSummary struct {
	Tool      string    `json:"tool"`
	SessionID string    `json:"session_id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int64     `json:"size"`
}

type aiSessionPullArgs struct {
	Path string `json:"path"`
	Tool string `json:"tool,omitempty"`
}

// AISessionBundle is one tool's bundled session files, as exchanged over the wire.
type AISessionBundle struct {
	Tool    string `json:"tool"`
	Tarball []byte `json:"tarball"`
}

func aiDecryptArgs(enc *encryptionKeys, inv *xconn.Invocation, out any) *xconn.InvocationResult {
	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	if err := json.Unmarshal(plaintext, out); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	return nil
}

func aiEncryptResult(enc *encryptionKeys, result any) *xconn.InvocationResult {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleAISessionList(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	var path string
	if errResult := aiDecryptArgs(enc, inv, &path); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(path)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	var summaries []AISessionSummary
	for _, s := range sessions {
		summaries = append(summaries, AISessionSummary{
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
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	var args aiSessionPullArgs
	if errResult := aiDecryptArgs(enc, inv, &args); errResult != nil {
		return errResult
	}

	sessions, err := aiLocalSessions(args.Path)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	byTool := make(map[string][]ai.SessionFile)
	for _, s := range sessions {
		if args.Tool != "" && s.Tool != args.Tool {
			continue
		}
		byTool[s.Tool] = append(byTool[s.Tool], s)
	}
	if len(byTool) == 0 {
		return xconn.NewInvocationError(ErrOperationFailed, "no local sessions found on this device for this project")
	}

	var bundles []AISessionBundle
	for tool, files := range byTool {
		tarball, err := ai.BuildTarball(files)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		bundles = append(bundles, AISessionBundle{Tool: tool, Tarball: tarball})
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

// aiProxyCall asks localSession's daemon to run procedure against realm's device on our
// behalf, reusing its cached (and, unless useP2P is set, auto-upgrading) device session
// instead of this process connecting fresh - the same ProcedureProxyFileOp path file
// operations already use.
func aiProxyCall(localSession *xconn.Session, realm, procedure string, payload []byte, useP2P bool) ([]byte, error) {
	call := localSession.Call(ProcedureProxyFileOp)
	if useP2P {
		call = call.Args(realm, procedure, payload, true)
	} else {
		call = call.Args(realm, procedure, payload)
	}
	resp := call.Do()
	if resp.Err != nil {
		return nil, resp.Err
	}
	return resp.ArgBytes(0)
}

func parseAISessionListResult(respBytes []byte) ([]AISessionSummary, error) {
	var sessions []AISessionSummary
	if err := json.Unmarshal(respBytes, &sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return sessions, nil
}

func CallAISessionList(deviceSession *xconn.Session, path string) ([]AISessionSummary, error) {
	payload, err := json.Marshal(path)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(deviceSession, ProcedureAISessionList, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionListResult(respBytes)
}

func CallAISessionListProxy(localSession *xconn.Session, realm, path string, useP2P bool) ([]AISessionSummary, error) {
	payload, err := json.Marshal(path)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := aiProxyCall(localSession, realm, ProcedureAISessionList, payload, useP2P)
	if err != nil {
		return nil, err
	}
	return parseAISessionListResult(respBytes)
}

func parseAISessionPullResult(respBytes []byte) ([]AISessionBundle, error) {
	var bundles []AISessionBundle
	if err := json.Unmarshal(respBytes, &bundles); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return bundles, nil
}

func CallAISessionPull(deviceSession *xconn.Session, path, tool string) ([]AISessionBundle, error) {
	payload, err := json.Marshal(aiSessionPullArgs{Path: path, Tool: tool})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(deviceSession, ProcedureAISessionPull, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionPullResult(respBytes)
}

func CallAISessionPullProxy(localSession *xconn.Session, realm, path, tool string,
	useP2P bool) ([]AISessionBundle, error) {
	payload, err := json.Marshal(aiSessionPullArgs{Path: path, Tool: tool})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := aiProxyCall(localSession, realm, ProcedureAISessionPull, payload, useP2P)
	if err != nil {
		return nil, err
	}
	return parseAISessionPullResult(respBytes)
}
