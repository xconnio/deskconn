package deskconn_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func tempDirInHome(t *testing.T) string {
	t.Helper()
	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)
	dir, err := os.MkdirTemp(homeDir, ".deskconn-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestFileBrowserBrowseEmptyPathReturnsHome(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse("", "", 0)
	require.NoError(t, err)
	require.True(t, result.IsDir)
	require.Equal(t, filepath.Clean(homeDir), result.Path)
	require.Equal(t, filepath.Clean(homeDir), result.HomePath)
}

func TestFileBrowserBrowseDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bb"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse(dir, "", 0)
	require.NoError(t, err)
	require.True(t, result.IsDir)
	require.Equal(t, "directory", result.Type)
	require.Equal(t, dir, result.Path)
	require.Equal(t, filepath.Dir(dir), result.ParentPath)
	require.Len(t, result.Entries, 2)
}

func TestFileBrowserBrowseFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello file")
	filePath := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse(filePath, "", 0)
	require.NoError(t, err)
	require.False(t, result.IsDir)
	require.Equal(t, "file", result.Type)
	require.EqualValues(t, len(content), result.Size)
	require.Empty(t, result.Entries)
}

func TestFileBrowserBrowseNonExistent(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	_, err := fb.Browse("/nonexistent/deskconn-test-path", "", 0)
	require.Error(t, err)
}

func TestFileBrowserBrowseEntriesSortedDirsFirst(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "z-dir"), 0755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-dir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "z.txt"), []byte("z"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse(dir, "", 0)
	require.NoError(t, err)
	require.Len(t, result.Entries, 4)

	// Directories come before files, each group sorted alphabetically.
	require.True(t, result.Entries[0].IsDir)
	require.Equal(t, "a-dir", result.Entries[0].Name)
	require.True(t, result.Entries[1].IsDir)
	require.Equal(t, "z-dir", result.Entries[1].Name)
	require.False(t, result.Entries[2].IsDir)
	require.Equal(t, "a.txt", result.Entries[2].Name)
	require.False(t, result.Entries[3].IsDir)
	require.Equal(t, "z.txt", result.Entries[3].Name)
}

func TestFileBrowserBrowsePagination(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-dir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0644))

	fb := deskconn.NewFileBrowser()

	page1, err := fb.Browse(dir, "", 2)
	require.NoError(t, err)
	require.Len(t, page1.Entries, 2)
	require.True(t, page1.HasMore)
	require.NotEmpty(t, page1.NextCursor)
	require.Equal(t, "a-dir", page1.Entries[0].Name)
	require.Equal(t, "a.txt", page1.Entries[1].Name)

	page2, err := fb.Browse(dir, page1.NextCursor, 2)
	require.NoError(t, err)
	require.Len(t, page2.Entries, 2)
	require.False(t, page2.HasMore)
	require.Empty(t, page2.NextCursor)
	require.Equal(t, "b.txt", page2.Entries[0].Name)
	require.Equal(t, "c.txt", page2.Entries[1].Name)
}

func TestFileBrowserBrowseHiddenEntry(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "visible"), []byte("v"), 0644))

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse(dir, "", 0)
	require.NoError(t, err)

	byName := make(map[string]deskconn.FileEntry, len(result.Entries))
	for _, e := range result.Entries {
		byName[e.Name] = e
	}
	require.True(t, byName[".hidden"].Hidden)
	require.False(t, byName["visible"].Hidden)
}

func TestFileBrowserBrowseSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("target"), 0644))
	require.NoError(t, os.Symlink(target, link))

	fb := deskconn.NewFileBrowser()
	result, err := fb.Browse(dir, "", 0)
	require.NoError(t, err)

	byName := make(map[string]deskconn.FileEntry, len(result.Entries))
	for _, e := range result.Entries {
		byName[e.Name] = e
	}
	linkEntry := byName["link.txt"]
	require.True(t, linkEntry.IsSymlink)
	require.Equal(t, target, linkEntry.LinkTarget)
}

func TestFileBrowserBrowseRelativePathEscapesHome(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	_, err := fb.Browse("../../etc", "", 0)
	require.ErrorContains(t, err, "relative path escapes home directory")
}

func TestFileBrowserRenameFile(t *testing.T) {
	dir := tempDirInHome(t)
	src := filepath.Join(dir, "original.txt")
	dst := filepath.Join(dir, "renamed.txt")
	require.NoError(t, os.WriteFile(src, []byte("content"), 0644))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Rename(src, dst))

	_, err := os.Stat(src)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(dst)
	require.NoError(t, err)
}

func TestFileBrowserRenameEmptyPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Rename("", "newname")
	require.ErrorContains(t, err, "path cannot be empty")
}

func TestFileBrowserRenamePathEscapesHome(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Rename("/etc/hosts", "/etc/hosts2")
	require.ErrorContains(t, err, "relative path escapes home directory")
}

func TestFileBrowserDeleteFile(t *testing.T) {
	dir := tempDirInHome(t)
	filePath := filepath.Join(dir, "to-delete.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("x"), 0644))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Delete(filePath))

	_, err := os.Stat(filePath)
	require.True(t, os.IsNotExist(err))
}

func TestFileBrowserDeleteDirectory(t *testing.T) {
	dir := tempDirInHome(t)
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("x"), 0644))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Delete(subDir))

	_, err := os.Stat(subDir)
	require.True(t, os.IsNotExist(err))
}

func TestFileBrowserDeleteHomeDirectoryBlocked(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	fb := deskconn.NewFileBrowser()
	err = fb.Delete(homeDir)
	require.ErrorContains(t, err, "cannot delete home directory")
}

func TestFileBrowserDeleteEmptyPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Delete("")
	require.ErrorContains(t, err, "path cannot be empty")
}

func TestFileBrowserDeletePathEscapesHome(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Delete("/etc/hosts")
	require.ErrorContains(t, err, "relative path escapes home directory")
}

func TestFileBrowserCopyFile(t *testing.T) {
	dir := tempDirInHome(t)
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	require.NoError(t, os.WriteFile(src, []byte("copy me"), 0644))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Copy(src, dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, []byte("copy me"), got)
}

func TestFileBrowserCopyDirectory(t *testing.T) {
	dir := tempDirInHome(t)
	srcDir := filepath.Join(dir, "src")
	dstDir := filepath.Join(dir, "dst")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("dir copy"), 0644))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Copy(srcDir, dstDir))

	got, err := os.ReadFile(filepath.Join(dstDir, "file.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("dir copy"), got)
}

func TestFileBrowserCopyNonExistentSource(t *testing.T) {
	dir := tempDirInHome(t)
	fb := deskconn.NewFileBrowser()
	err := fb.Copy(filepath.Join(dir, "ghost.txt"), filepath.Join(dir, "dst.txt"))
	require.Error(t, err)
}

func TestFileBrowserCopyEmptyPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Copy("", "dst")
	require.ErrorContains(t, err, "path cannot be empty")
}

func TestFileBrowserCopyPathEscapesHome(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Copy("/etc/hosts", "/etc/hosts2")
	require.ErrorContains(t, err, "relative path escapes home directory")
}

func TestFileBrowserEditAppliesPatch(t *testing.T) {
	dir := tempDirInHome(t)
	path := filepath.Join(dir, "edit.txt")
	original := []byte("hello world\n")
	require.NoError(t, os.WriteFile(path, original, 0644))

	edited := []byte("hello there world\n")
	patch := deskconn.BuildEditPatch(original, edited)

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Edit(path, patch))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, edited, got)
}

func TestFileBrowserEditPreservesFileMode(t *testing.T) {
	dir := tempDirInHome(t)
	path := filepath.Join(dir, "edit.sh")
	original := []byte("echo one\n")
	require.NoError(t, os.WriteFile(path, original, 0755))

	patch := deskconn.BuildEditPatch(original, []byte("echo two\n"))

	fb := deskconn.NewFileBrowser()
	require.NoError(t, fb.Edit(path, patch))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestFileBrowserEditRejectsBinaryFile(t *testing.T) {
	dir := tempDirInHome(t)
	path := filepath.Join(dir, "video.mp4")
	original := []byte("fake\x00mp4\x00data")
	require.NoError(t, os.WriteFile(path, original, 0644))

	fb := deskconn.NewFileBrowser()
	err := fb.Edit(path, "")
	require.ErrorContains(t, err, "binary file, editing is not supported")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestIsEditableExtension(t *testing.T) {
	require.True(t, deskconn.IsEditableExtension("notes.txt"))
	require.True(t, deskconn.IsEditableExtension("main.go"))
	require.True(t, deskconn.IsEditableExtension("Makefile"))
	require.True(t, deskconn.IsEditableExtension(".gitignore"))

	require.False(t, deskconn.IsEditableExtension("movie.mp4"))
	require.False(t, deskconn.IsEditableExtension("photo.jpg"))
	require.False(t, deskconn.IsEditableExtension("report.pdf"))
	require.False(t, deskconn.IsEditableExtension("resume.docx"))
}

func TestFileBrowserEditConflictLeavesFileUntouched(t *testing.T) {
	dir := tempDirInHome(t)
	path := filepath.Join(dir, "edit.txt")
	original := []byte("hello world\n")
	require.NoError(t, os.WriteFile(path, original, 0644))

	patch := deskconn.BuildEditPatch(original, []byte("hello there world\n"))

	changed := []byte("something else entirely\n")
	require.NoError(t, os.WriteFile(path, changed, 0644))

	fb := deskconn.NewFileBrowser()
	err := fb.Edit(path, patch)
	require.ErrorContains(t, err, "patch could not be applied cleanly")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, changed, got)
}

func TestFileBrowserEditEmptyPath(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Edit("", "")
	require.ErrorContains(t, err, "path cannot be empty")
}

func TestFileBrowserEditPathEscapesHome(t *testing.T) {
	fb := deskconn.NewFileBrowser()
	err := fb.Edit("/etc/hosts", "")
	require.ErrorContains(t, err, "relative path escapes home directory")
}

func TestFileBrowserEditDirectory(t *testing.T) {
	dir := tempDirInHome(t)
	fb := deskconn.NewFileBrowser()
	err := fb.Edit(dir, "")
	require.ErrorContains(t, err, "is a directory")
}

func TestFileBrowserEditNonExistent(t *testing.T) {
	dir := tempDirInHome(t)
	fb := deskconn.NewFileBrowser()
	err := fb.Edit(filepath.Join(dir, "ghost.txt"), "")
	require.Error(t, err)
}

func TestHandleFileEditRPC(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))

	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	d := deskconn.NewDeskconn(nil, nil, nil, false)
	require.NoError(t, d.Register(callee))

	dir := tempDirInHome(t)
	path := filepath.Join(dir, "rpc-edit.txt")
	original := []byte("hello world\n")
	require.NoError(t, os.WriteFile(path, original, 0644))

	edited := []byte("hello there world\n")
	patch := deskconn.BuildEditPatch(original, edited)

	payload, err := json.Marshal(map[string]string{"path": path, "patch": patch})
	require.NoError(t, err)

	_, err = deskconn.CallFileOp(caller, deskconn.ProcedureFileEdit, payload)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, edited, got)
}

func TestBuildEditPatchNoChanges(t *testing.T) {
	content := []byte("unchanged content\n")
	patch := deskconn.BuildEditPatch(content, content)

	fb := deskconn.NewFileBrowser()
	dir := tempDirInHome(t)
	path := filepath.Join(dir, "unchanged.txt")
	require.NoError(t, os.WriteFile(path, content, 0644))

	require.NoError(t, fb.Edit(path, patch))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, got)
}
