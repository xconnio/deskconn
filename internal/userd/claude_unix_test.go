//go:build unix

package userd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

// claudeProjectDir mirrors the directory-name encoding DiscoverClaudeSessions expects under
// ~/.claude/projects/: the full absolute path (homeDir+path), with every "/" replaced by "-".
func claudeProjectDir(homeDir, path string) string {
	return strings.ReplaceAll(filepath.Join(homeDir, path), "/", "-")
}

// seedClaudeSession writes a Claude session file (with a summary line, like Claude Code's own
// session picker relies on) under homeDir for the project at path.
func seedClaudeSession(t *testing.T, homeDir, path string) {
	t.Helper()
	seedClaudeSessionWithID(t, homeDir, path, "abc")
}

// seedClaudeSessionWithID is seedClaudeSession with an explicit session id (jsonl basename), so
// tests can seed more than one session for the same project.
func seedClaudeSessionWithID(t *testing.T, homeDir, path, sessionID string) {
	t.Helper()
	projectDir := filepath.Join(homeDir, ".claude", "projects", claudeProjectDir(homeDir, path))
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	content := `{"type":"summary","summary":"Fix the login bug","leafUuid":"x"}` + "\n" + `{"hello":"world"}`
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(content), 0600))
}

func TestClaudeSessionListHandlerReturnsLocalSessions(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSession(t, homeDir, path)

	sessions, err := deskconn.CallAgentSessionList(caller, common.ClaudeTool, path)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, common.ClaudeTool, sessions[0].Tool)
	require.Equal(t, "abc", sessions[0].SessionID)
	require.Equal(t, "Fix the login bug", sessions[0].Title)
}

func TestClaudeSessionPullHandlerReturnsBundle(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSession(t, homeDir, path)

	bundles, err := deskconn.CallAgentSessionPull(caller, common.ClaudeTool, path, "")
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, common.ClaudeTool, bundles[0].Tool)

	// Extracting onto a different "machine" (a different home directory, standing in for a
	// different username) must still land under that machine's own correctly re-encoded
	// project directory, not the source's.
	restoreHome := t.TempDir()
	count, err := deskconn.ExtractAgentTarball(common.ClaudeTool, bundles[0].Tarball, restoreHome, path)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	_, err = os.Stat(filepath.Join(restoreHome, ".claude", "projects",
		claudeProjectDir(restoreHome, path), "abc.jsonl"))
	require.NoError(t, err)
}

func TestClaudeSessionPullHandlerFiltersBySessionIDPrefix(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSessionWithID(t, homeDir, path, "abc123")
	seedClaudeSessionWithID(t, homeDir, path, "def456")

	bundles, err := deskconn.CallAgentSessionPull(caller, common.ClaudeTool, path, "abc")
	require.NoError(t, err)
	require.Len(t, bundles, 1)

	restoreHome := t.TempDir()
	count, err := deskconn.ExtractAgentTarball(common.ClaudeTool, bundles[0].Tarball, restoreHome, path)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	_, err = os.Stat(filepath.Join(restoreHome, ".claude", "projects",
		claudeProjectDir(restoreHome, path), "abc123.jsonl"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(restoreHome, ".claude", "projects",
		claudeProjectDir(restoreHome, path), "def456.jsonl"))
	require.True(t, os.IsNotExist(err))
}

func TestClaudeSessionPullHandlerSessionIDNoMatchErrors(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSessionWithID(t, homeDir, path, "abc123")

	_, err := deskconn.CallAgentSessionPull(caller, common.ClaudeTool, path, "zzz")
	require.ErrorContains(t, err, `no session matching "zzz"`)
}

func TestClaudeSessionPullHandlerSessionIDAmbiguousPrefixErrors(t *testing.T) {
	homeDir := isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	path := randomPath(t)
	seedClaudeSessionWithID(t, homeDir, path, "abc123")
	seedClaudeSessionWithID(t, homeDir, path, "abc456")

	_, err := deskconn.CallAgentSessionPull(caller, common.ClaudeTool, path, "abc")
	require.ErrorContains(t, err, `"abc" matches more than one session; use a longer prefix`)
}
