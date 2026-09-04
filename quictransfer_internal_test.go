package deskconn

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFileName = "file.txt"

// newQUICTestStream returns the client end of a net.Pipe whose server end is
// being served by HandleQUICStream, exercising the exact same dispatch and
// handlers a real QUIC stream from sess.OpenStream() would hit -- net.Pipe
// satisfies net.Conn, which is all HandleQUICStream requires.
func newQUICTestStream(t *testing.T) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	var d Deskconn
	go d.HandleQUICStream(nil, server)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// sendReq writes req as a real client would: a leading routingFrame (which
// HandleQUICStream always discards -- see its doc comment) followed by the
// real request.
func sendReq(t *testing.T, conn net.Conn, req fsRequest) {
	t.Helper()
	require.NoError(t, writeMsg(conn, routingFrame{Op: req.Op, Path: req.Path, Recursive: req.Recursive}))
	require.NoError(t, writeMsg(conn, req))
}

func TestQUICHandleListSingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello world"), 0644))

	client := newQUICTestStream(t)
	sendReq(t, client, fsRequest{Op: fsOpList, Path: filePath})

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	require.True(t, resp.OK)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "hello.txt", resp.Entries[0].RelPath)
	assert.EqualValues(t, 11, resp.Entries[0].Size)
	assert.False(t, resp.Entries[0].IsDir)
}

func TestQUICHandleListDirRecursive(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0644))

	client := newQUICTestStream(t)
	sendReq(t, client, fsRequest{Op: fsOpList, Path: srcDir, Recursive: true})

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	require.True(t, resp.OK)
	require.Len(t, resp.Entries, 2)
}

func TestQUICHandleListDirWithoutRecursiveFails(t *testing.T) {
	dir := t.TempDir()

	client := newQUICTestStream(t)
	sendReq(t, client, fsRequest{Op: fsOpList, Path: dir})

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Err, "is a directory")
}

func TestQUICHandleListNonExistent(t *testing.T) {
	client := newQUICTestStream(t)
	sendReq(t, client, fsRequest{Op: fsOpList, Path: filepath.Join(t.TempDir(), "nope")})

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleReadRange(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "data.bin")
	content := []byte("0123456789ABCDEF")
	require.NoError(t, os.WriteFile(filePath, content, 0644))

	client := newQUICTestStream(t)
	req := fsRequest{Op: fsOpRead, Path: filePath, RelPath: "data.bin", Offset: 3, Length: 5}
	sendReq(t, client, req)

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	require.True(t, resp.OK)

	buf := make([]byte, 5)
	_, err := io.ReadFull(client, buf)
	require.NoError(t, err)
	assert.Equal(t, content[3:8], buf)
}

func TestQUICHandleReadNonExistentFile(t *testing.T) {
	dir := t.TempDir()

	client := newQUICTestStream(t)
	req := fsRequest{Op: fsOpRead, Path: dir, RelPath: "missing.txt", Offset: 0, Length: 1}
	sendReq(t, client, req)

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleInitAndWrite(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	entries := []transferManifestEntry{
		{RelPath: testFileName, Size: 10, Mode: 0644},
	}

	initClient := newQUICTestStream(t)
	sendReq(t, initClient, fsRequest{
		Op: fsOpInit, Path: dst, Entries: entries, TargetIsDirHint: true,
	})
	var initResp fsResponse
	require.NoError(t, readMsg(initClient, &initResp))
	require.True(t, initResp.OK)

	fi, err := os.Stat(filepath.Join(dst, testFileName))
	require.NoError(t, err)
	assert.EqualValues(t, 10, fi.Size())

	writeClient := newQUICTestStream(t)
	writeReq := fsRequest{
		Op: fsOpWrite, Path: dst, RelPath: testFileName, Offset: 2, Length: 5, TargetIsDirHint: true,
	}
	sendReq(t, writeClient, writeReq)

	var ack fsResponse
	require.NoError(t, readMsg(writeClient, &ack))
	require.True(t, ack.OK)

	_, err = writeClient.Write([]byte("HELLO"))
	require.NoError(t, err)

	var final fsResponse
	require.NoError(t, readMsg(writeClient, &final))
	require.True(t, final.OK)

	got, err := os.ReadFile(filepath.Join(dst, testFileName))
	require.NoError(t, err)
	want := make([]byte, 10)
	copy(want[2:7], "HELLO")
	assert.Equal(t, want, got)
}

func TestQUICHandleWriteBeforeInitFails(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	client := newQUICTestStream(t)
	req := fsRequest{Op: fsOpWrite, Path: dst, RelPath: testFileName, Offset: 0, Length: 5, TargetIsDirHint: true}
	sendReq(t, client, req)

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleInitSingleFileTarget(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "renamed.txt")

	entries := []transferManifestEntry{
		{RelPath: "original.txt", Size: 4, Mode: 0644},
	}

	client := newQUICTestStream(t)
	sendReq(t, client, fsRequest{Op: fsOpInit, Path: dst, Entries: entries})

	var resp fsResponse
	require.NoError(t, readMsg(client, &resp))
	require.True(t, resp.OK)

	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.False(t, fi.IsDir())
	assert.EqualValues(t, 4, fi.Size())
}
