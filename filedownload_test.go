package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestFileDownloadMissingPath(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedureFileDownload).Do()
	require.ErrorContains(t, callResp.Err, "wamp.error.invalid_argument")
}

func TestFileDownloadMissingPublicKey(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedureFileDownload).Args("/tmp/test").Do()
	require.ErrorContains(t, callResp.Err, "client public key is required")
}

func TestFileDownloadInvalidPublicKeyLength(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedureFileDownload).
		Args("/tmp/test", false, []byte("tooshort")).Do()
	require.ErrorContains(t, callResp.Err, "client public key is required")
}

func TestPullFilesNonExistentPath(t *testing.T) {
	_, caller := setupDeskconn(t)
	err := deskconn.PullFiles(caller, "/nonexistent/path/that/does/not/exist", t.TempDir(), false)
	require.ErrorContains(t, err, "no such file or directory")
}

func TestPullFilesDirWithoutRecursive(t *testing.T) {
	_, caller := setupDeskconn(t)
	err := deskconn.PullFiles(caller, t.TempDir(), t.TempDir(), false)
	require.ErrorContains(t, err, "is a directory")
}

func TestPullFilesEmptyFile(t *testing.T) {
	_, caller := setupDeskconn(t)

	srcFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte{}, 0644))

	dstFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, deskconn.PullFiles(caller, srcFile, dstFile, false))

	info, err := os.Stat(dstFile)
	require.NoError(t, err)
	require.EqualValues(t, 0, info.Size())
}

func TestPullFilesSingleFile(t *testing.T) {
	_, caller := setupDeskconn(t)

	content := []byte("hello from the other side")
	srcFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, deskconn.PullFiles(caller, srcFile, dstFile, false))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestPullFilesDirectory(t *testing.T) {
	_, caller := setupDeskconn(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, deskconn.PullFiles(caller, srcDir, dstDir, true))

	base := filepath.Base(srcDir)

	got, err := os.ReadFile(filepath.Join(dstDir, base, "root.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("root content"), got)

	got, err = os.ReadFile(filepath.Join(dstDir, base, "subdir", "nested.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("nested content"), got)
}
