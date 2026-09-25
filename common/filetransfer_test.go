package common_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

func TestBuildManifestSingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0644))

	entries, err := common.BuildManifest(filePath, false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "hello.txt", entries[0].RelPath)
	assert.EqualValues(t, 5, entries[0].Size)
	assert.False(t, entries[0].IsDir)
}

func TestBuildManifestDirWithoutRecursiveFails(t *testing.T) {
	dir := t.TempDir()
	_, err := common.BuildManifest(dir, false)
	require.ErrorContains(t, err, "is a directory")
}

func TestBuildManifestNonExistentPath(t *testing.T) {
	_, err := common.BuildManifest(filepath.Join(t.TempDir(), "nope"), false)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func TestBuildManifestRecursiveDir(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested content"), 0644))

	entries, err := common.BuildManifest(srcDir, true)
	require.NoError(t, err)

	byRel := map[string]common.TransferManifestEntry{}
	for _, e := range entries {
		byRel[e.RelPath] = e
	}

	require.Contains(t, byRel, "src")
	assert.True(t, byRel["src"].IsDir)
	require.Contains(t, byRel, "src/sub")
	assert.True(t, byRel["src/sub"].IsDir)
	require.Contains(t, byRel, "src/root.txt")
	assert.EqualValues(t, 4, byRel["src/root.txt"].Size)
	require.Contains(t, byRel, "src/sub/nested.txt")
	assert.EqualValues(t, 14, byRel["src/sub/nested.txt"].Size)
}

func TestIsRootDir(t *testing.T) {
	dir := t.TempDir()

	existingDir := filepath.Join(dir, "adir")
	require.NoError(t, os.Mkdir(existingDir, 0755))
	existingFile := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(existingFile, []byte("x"), 0644))
	missing := filepath.Join(dir, "missing")

	assert.True(t, common.IsRootDir(existingDir, false, false))
	assert.False(t, common.IsRootDir(existingFile, false, false))
	assert.False(t, common.IsRootDir(missing, false, false))
	assert.True(t, common.IsRootDir(missing, true, false), "sourceIsDir should force directory treatment")
	assert.True(t, common.IsRootDir(missing, false, true), "targetIsDirHint should force directory treatment")
	assert.True(t, common.IsRootDir(existingFile, true, false), "sourceIsDir overrides an existing non-dir target")
}

func TestResolveDestPath(t *testing.T) {
	// rootIsDir: entries nest under root by their full RelPath.
	got := common.ResolveDestPath("/dst", true, "src", "src/sub/file.txt")
	assert.Equal(t, filepath.Clean("/dst/src/sub/file.txt"), got)

	// !rootIsDir: the transfer's top-level entry is renamed to root, and
	// descendants nest under root by the remainder of their RelPath.
	got = common.ResolveDestPath("/dst/renamed", false, "src", "src")
	assert.Equal(t, filepath.Clean("/dst/renamed"), got)

	got = common.ResolveDestPath("/dst/renamed", false, "src", "src/sub/file.txt")
	assert.Equal(t, filepath.Clean("/dst/renamed/sub/file.txt"), got)
}

func TestMaterializeTargetsCreatesDirsAndPreSizedFiles(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	entries := []common.TransferManifestEntry{
		{RelPath: "top", IsDir: true, Mode: 0755},
		{RelPath: "top/sub", IsDir: true, Mode: 0755},
		{RelPath: "top/file.txt", Size: 123, Mode: 0644},
		{RelPath: "top/sub/nested.txt", Size: 7, Mode: 0644},
	}

	require.NoError(t, common.MaterializeTargets(entries, dst, true))

	info, err := os.Stat(filepath.Join(dst, "top", "sub"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	fi, err := os.Stat(filepath.Join(dst, "top", "file.txt"))
	require.NoError(t, err)
	assert.EqualValues(t, 123, fi.Size())

	fi, err = os.Stat(filepath.Join(dst, "top", "sub", "nested.txt"))
	require.NoError(t, err)
	assert.EqualValues(t, 7, fi.Size())
}

func TestMaterializeTargetsSingleFileRename(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "renamed.txt")

	entries := []common.TransferManifestEntry{
		{RelPath: "original.txt", Size: 42, Mode: 0644},
	}

	require.NoError(t, common.MaterializeTargets(entries, dst, false))

	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.EqualValues(t, 42, fi.Size())
}

func TestRecvPriorityPrefersBufferedValue(t *testing.T) {
	ch := make(chan int, 1)
	closed := make(chan struct{})
	ch <- 42
	close(closed) // closed is also ready, but the buffered value must win

	v, err := common.RecvPriority(ch, closed, time.Second)
	require.NoError(t, err)
	assert.Equal(t, 42, v)
}

func TestRecvPriorityReturnsErrorOnClose(t *testing.T) {
	ch := make(chan int)
	closed := make(chan struct{})
	close(closed)

	_, err := common.RecvPriority(ch, closed, time.Second)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}
