package deskconnd_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

const (
	testProjectPath = "code/project"
	typeKey         = "type"
	typeSummary     = "summary"
	typeUser        = "user"
)

func jsonLine(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return append(b, '\n')
}

func TestDiscoverClaudeSessionsMissingDirReturnsEmpty(t *testing.T) {
	homeDir := t.TempDir()
	sessions, err := common.DiscoverClaudeSessions(homeDir, "/some/repo")
	require.NoError(t, err)
	require.Empty(t, sessions)
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

	require.Equal(t, "Fix the login bug", common.AISessionTitle(path))
}

func TestSessionTitleFallsBackToFirstUserMessage(t *testing.T) {
	homeDir := t.TempDir()
	path := filepath.Join(homeDir, "session.jsonl")

	buf := jsonLine(t, map[string]any{
		typeKey: typeUser, "message": map[string]any{"role": typeUser, "content": "please refactor the parser"},
	})
	require.NoError(t, os.WriteFile(path, buf, 0600))

	require.Equal(t, "please refactor the parser", common.AISessionTitle(path))
}
