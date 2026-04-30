package deskconn_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func TestFileUploadNoProgress(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedureFileUpload).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestFileUploadMissingKeyExchange(t *testing.T) {
	_, caller := setupDeskconn(t)

	sent := false
	callResp := caller.Call(deskconn.ProcedureFileUpload).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("I") // msgInit without prior key exchange
			}
			return xconn.NewFinalProgress()
		}).Do()

	require.ErrorContains(t, callResp.Err, "first upload progress must perform key exchange")
}

func TestPushFilesNonExistentLocalPath(t *testing.T) {
	_, caller := setupDeskconn(t)
	err := deskconn.PushFiles(caller, "/nonexistent/local/path", t.TempDir(), false)
	require.ErrorContains(t, err, "no such file or directory")
}

func TestPushFilesDirWithoutRecursive(t *testing.T) {
	_, caller := setupDeskconn(t)
	err := deskconn.PushFiles(caller, t.TempDir(), t.TempDir(), false)
	require.ErrorContains(t, err, "is a directory")
}

func TestPushFilesEmptyFile(t *testing.T) {
	_, caller := setupDeskconn(t)

	srcFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, os.WriteFile(srcFile, []byte{}, 0644))

	dstFile := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, deskconn.PushFiles(caller, srcFile, dstFile, false))

	info, err := os.Stat(dstFile)
	require.NoError(t, err)
	require.EqualValues(t, 0, info.Size())
}

func TestPushFilesSingleFile(t *testing.T) {
	_, caller := setupDeskconn(t)

	content := []byte("hello from push")
	srcFile := filepath.Join(t.TempDir(), "push.txt")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "push.txt")
	require.NoError(t, deskconn.PushFiles(caller, srcFile, dstFile, false))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestPushFilesDirectory(t *testing.T) {
	_, caller := setupDeskconn(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, deskconn.PushFiles(caller, srcDir, dstDir, true))

	base := filepath.Base(srcDir)

	got, err := os.ReadFile(filepath.Join(dstDir, base, "root.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("root content"), got)

	got, err = os.ReadFile(filepath.Join(dstDir, base, "subdir", "nested.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("nested content"), got)
}
