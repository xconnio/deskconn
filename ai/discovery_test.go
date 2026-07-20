package ai_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/ai"
)

const (
	testProjectPath = "code/project"
	typeKey         = "type"
	typeSummary     = "summary"
	typeUser        = "user"
)

func writeFile(t *testing.T, path string, content []byte, modTime time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, content, 0600))
	require.NoError(t, os.Chtimes(path, modTime, modTime))
}

// encodedProjectDir mirrors Claude Code's own directory-name encoding for the project at path
// (relative to homeDir): the full absolute path, with every "/" replaced by "-".
func encodedProjectDir(homeDir, path string) string {
	return strings.ReplaceAll(filepath.Join(homeDir, path), string(filepath.Separator), "-")
}

func jsonLine(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return append(b, '\n')
}

func TestDiscoverClaudeSessionsMissingDirReturnsEmpty(t *testing.T) {
	homeDir := t.TempDir()
	sessions, err := ai.DiscoverClaudeSessions(homeDir, "/some/repo")
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestDiscoverClaudeSessionsFindsMatchingProjectSortedNewestFirst(t *testing.T) {
	homeDir := t.TempDir()
	path := testProjectPath
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodedProjectDir(homeDir, path))

	now := time.Now()
	writeFile(t, filepath.Join(projectDir, "older.jsonl"), []byte(`{}`), now.Add(-time.Hour))
	writeFile(t, filepath.Join(projectDir, "newer.jsonl"), []byte(`{}`), now)
	// A file for a different, unrelated project should never surface.
	otherDir := filepath.Join(homeDir, ".claude", "projects", encodedProjectDir(homeDir, "code/other"))
	writeFile(t, filepath.Join(otherDir, "unrelated.jsonl"), []byte(`{}`), now)

	sessions, err := ai.DiscoverClaudeSessions(homeDir, path)
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	require.Equal(t, "newer.jsonl", filepath.Base(sessions[0].Path))
	require.Equal(t, "older.jsonl", filepath.Base(sessions[1].Path))
	require.Equal(t, ai.ToolClaude, sessions[0].Tool)
}

func TestDiscoverClaudeSessionsIndependentOfHomeDir(t *testing.T) {
	path := testProjectPath

	homeA := t.TempDir()
	dirA := filepath.Join(homeA, ".claude", "projects", encodedProjectDir(homeA, path))
	writeFile(t, filepath.Join(dirA, "a.jsonl"), []byte(`{}`), time.Now())

	homeB := t.TempDir()
	dirB := filepath.Join(homeB, ".claude", "projects", encodedProjectDir(homeB, path))
	writeFile(t, filepath.Join(dirB, "b.jsonl"), []byte(`{}`), time.Now())

	sessionsA, err := ai.DiscoverClaudeSessions(homeA, path)
	require.NoError(t, err)
	require.Len(t, sessionsA, 1)
	require.Equal(t, "a.jsonl", filepath.Base(sessionsA[0].Path))

	sessionsB, err := ai.DiscoverClaudeSessions(homeB, path)
	require.NoError(t, err)
	require.Len(t, sessionsB, 1)
	require.Equal(t, "b.jsonl", filepath.Base(sessionsB[0].Path))
}

func TestSessionTitleUsesSummaryLine(t *testing.T) {
	homeDir := t.TempDir()
	path := filepath.Join(homeDir, "session.jsonl")

	var buf []byte
	buf = append(buf, jsonLine(t, map[string]any{
		typeKey: typeSummary, "summary": "Fix the login bug", "leafUuid": "x",
	})...)
	buf = append(buf, jsonLine(t, map[string]any{
		typeKey: typeUser, "message": map[string]any{"role": typeUser, "content": "hello"},
	})...)
	require.NoError(t, os.WriteFile(path, buf, 0600))

	require.Equal(t, "Fix the login bug", ai.SessionTitle(path))
}

func TestSessionTitleFallsBackToFirstUserMessage(t *testing.T) {
	homeDir := t.TempDir()
	path := filepath.Join(homeDir, "session.jsonl")

	buf := jsonLine(t, map[string]any{
		typeKey: typeUser, "message": map[string]any{"role": typeUser, "content": "please refactor the parser"},
	})
	require.NoError(t, os.WriteFile(path, buf, 0600))

	require.Equal(t, "please refactor the parser", ai.SessionTitle(path))
}
