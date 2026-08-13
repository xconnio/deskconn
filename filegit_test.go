package deskconn_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	return dir
}

func TestGitStatusNonRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.False(t, result.IsRepo)
	require.Empty(t, result.Entries)
}

func TestGitStatusNonExistentPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	_, err := fb.GitStatus("/nonexistent/deskconn-git-test-path")
	require.Error(t, err)
}

func TestGitStatusCleanRepoNoEntries(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.Equal(t, dir, result.RepoRoot)
	require.Empty(t, result.Entries)
}

func TestGitStatusUntrackedFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filepath.Join(dir, "new.txt"), result.Entries[0].Path)
	require.Equal(t, "untracked", result.Entries[0].Status)
}

func TestGitStatusIgnoredFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0644))
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add gitignore")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filepath.Join(dir, "ignored.txt"), result.Entries[0].Path)
	require.Equal(t, "ignored", result.Entries[0].Status)
}

func TestGitStatusStagedNewFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hi"), 0644))
	runGit(t, dir, "add", "new.txt")

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filepath.Join(dir, "new.txt"), result.Entries[0].Path)
	require.Equal(t, "added", result.Entries[0].Status)
}

func TestGitStatusCommittedThenModifiedFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	filePath := filepath.Join(dir, "tracked.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("v1"), 0644))
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add tracked.txt")

	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filePath, result.Entries[0].Path)
	require.Equal(t, "modified", result.Entries[0].Status)
}

func TestGitStatusCommittedThenStagedModifiedFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	filePath := filepath.Join(dir, "tracked.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("v1"), 0644))
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add tracked.txt")

	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0644))
	runGit(t, dir, "add", "tracked.txt")

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(dir)
	require.NoError(t, err)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filePath, result.Entries[0].Path)
	require.Equal(t, "modified", result.Entries[0].Status)
}

func TestGitStatusFromSubdirectoryResolvesRepoRoot(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	subDir := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(subDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "new.txt"), []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitStatus(subDir)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.Equal(t, dir, result.RepoRoot)
	require.Len(t, result.Entries, 1)
	require.Equal(t, filepath.Join(subDir, "new.txt"), result.Entries[0].Path)
}

func TestGitOriginalCommittedFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	filePath := filepath.Join(dir, "tracked.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("original content"), 0644))
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add tracked.txt")

	// Modify on disk after the commit — GitOriginal must return the committed
	// version, not the current on-disk content.
	require.NoError(t, os.WriteFile(filePath, []byte("changed content"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitOriginal(filePath)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.False(t, result.IsNew)
	require.Equal(t, "original content", result.Content)
}

func TestGitOriginalUntrackedFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	filePath := filepath.Join(dir, "new.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitOriginal(filePath)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.True(t, result.IsNew)
	require.True(t, result.Untracked)
	require.Empty(t, result.Content)
}

func TestGitOriginalIgnoredFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0644))
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "add gitignore")
	filePath := filepath.Join(dir, "ignored.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitOriginal(filePath)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.True(t, result.IsNew)
	require.False(t, result.Untracked)
	require.True(t, result.Ignored)
	require.Empty(t, result.Content)
}

func TestGitOriginalStagedNewFile(t *testing.T) {
	requireGit(t)
	dir := initGitRepo(t)
	filePath := filepath.Join(dir, "staged.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0644))
	runGit(t, dir, "add", "staged.txt")

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitOriginal(filePath)
	require.NoError(t, err)
	require.True(t, result.IsRepo)
	require.True(t, result.IsNew)
	require.False(t, result.Untracked)
	require.Empty(t, result.Content)
}

func TestGitOriginalNonRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	filePath := filepath.Join(dir, "plain.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hi"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.GitOriginal(filePath)
	require.NoError(t, err)
	require.False(t, result.IsRepo)
	require.True(t, result.IsNew)
}

func TestGitOriginalNonExistentPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	_, err := fb.GitOriginal("/nonexistent/deskconn-git-test-path")
	require.Error(t, err)
}
