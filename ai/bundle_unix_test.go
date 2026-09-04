//go:build unix

package ai_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/ai"
)

// TestBuildAndExtractTarballRoundTrip also locks in the portability fix: the tarball is built
// on one "machine" (homeDir) and extracted onto a completely different one (restoreHome, a
// different temp dir standing in for a different username/home location), and the file must
// still land under restoreHome's own correctly re-encoded project directory - not homeDir's.
func TestBuildAndExtractTarballRoundTrip(t *testing.T) {
	homeDir := t.TempDir()
	path := testProjectPath
	projectDir := filepath.Join(homeDir, ".claude", "projects", encodedProjectDir(homeDir, path))
	writeFile(t, filepath.Join(projectDir, "session.jsonl"), []byte(`{"hello":"world"}`), time.Now())

	sessions, err := ai.DiscoverClaudeSessions(homeDir, path)
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	tarball, err := ai.BuildTarball(sessions)
	require.NoError(t, err)

	restoreHome := t.TempDir()
	count, err := ai.ExtractTarball(tarball, restoreHome, path)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	restored, err := os.ReadFile(filepath.Join(restoreHome, ".claude", "projects",
		encodedProjectDir(restoreHome, path), "session.jsonl"))
	require.NoError(t, err)
	require.Equal(t, `{"hello":"world"}`, string(restored))
}
