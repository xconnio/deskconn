package deskconn_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/deskconn/ai"
	"github.com/xconnio/xconn-go"
)

// claudeProjectDir mirrors the directory-name encoding DiscoverClaudeSessions expects under
// ~/.claude/projects/: the full absolute path (homeDir+path), with every "/" replaced by "-".
func claudeProjectDir(homeDir, path string) string {
	return strings.ReplaceAll(filepath.Join(homeDir, path), "/", "-")
}

func setupDeskconnWithInstance(t *testing.T) *xconn.Session {
	t.Helper()
	callee, caller := setupRouterAndConnectSessions(t)
	d := deskconn.NewDeskconn(nil, nil, nil)
	require.NoError(t, d.Register(callee))
	return caller
}

// randomPath returns a random project path relative to $HOME - production code always treats
// the project path this way, never as an absolute path, so tests must too.
func randomPath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("ai-rpc-test-%d", time.Now().UnixNano())
}

// isolatedHome points $HOME at a fresh temp directory for the duration of the test, so the RPC
// handlers under test (which always resolve os.UserHomeDir() for real) operate against a
// throwaway home instead of the real one.
func isolatedHome(t *testing.T) string {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	return homeDir
}

// seedClaudeSession writes a Claude session file (with a summary line, like Claude Code's own
// session picker relies on) under homeDir for the project at path.
func seedClaudeSession(t *testing.T, homeDir, path string) {
	t.Helper()
	projectDir := filepath.Join(homeDir, ".claude", "projects", claudeProjectDir(homeDir, path))
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	content := `{"type":"summary","summary":"Fix the login bug","leafUuid":"x"}` + "\n" + `{"hello":"world"}`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "abc.jsonl"), []byte(content), 0600))
}

func TestAISessionListHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(deskconn.ProcedureAISessionList).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestAISessionListHandlerNoMatchingSessionsReturnsEmpty(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	sessions, err := deskconn.CallAISessionList(caller, randomPath(t))
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestAISessionListHandlerReturnsLocalSessions(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSession(t, homeDir, path)

	sessions, err := deskconn.CallAISessionList(caller, path)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, ai.ToolClaude, sessions[0].Tool)
	require.Equal(t, "abc", sessions[0].SessionID)
	require.Equal(t, "Fix the login bug", sessions[0].Title)
}

func TestAISessionPullHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(deskconn.ProcedureAISessionPull).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestAISessionPullHandlerNoMatchingSessionsErrors(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	_, err := deskconn.CallAISessionPull(caller, randomPath(t), "")
	require.ErrorContains(t, err, "no local sessions found")
}

func TestAISessionPullHandlerReturnsBundle(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSession(t, homeDir, path)

	bundles, err := deskconn.CallAISessionPull(caller, path, "")
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, ai.ToolClaude, bundles[0].Tool)

	// Extracting onto a different "machine" (a different home directory, standing in for a
	// different username) must still land under that machine's own correctly re-encoded
	// project directory, not the source's.
	restoreHome := t.TempDir()
	count, err := ai.ExtractTarball(bundles[0].Tarball, restoreHome, path)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	_, err = os.Stat(filepath.Join(restoreHome, ".claude", "projects",
		claudeProjectDir(restoreHome, path), "abc.jsonl"))
	require.NoError(t, err)
}
