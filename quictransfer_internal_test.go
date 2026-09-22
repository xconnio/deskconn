package deskconn

import (
	"bytes"
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
	go func() {
		op, err := ReadStreamOp(server)
		if err != nil {
			return
		}
		d.DispatchQUICOp(op, server)
	}()
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// sendReq writes req as a real client would: a leading RoutingFrame (which
// HandleQUICStream always discards -- see its doc comment), then performs
// the per-stream key exchange, then sends the real request encrypted.
// Returns the derived keys so the caller can decrypt the response and any
// data that follows on the stream.
func sendReq(t *testing.T, conn net.Conn, req FSRequest) (sendKey, receiveKey []byte) {
	t.Helper()
	require.NoError(t, WriteMsg(conn,
		RoutingFrame{Op: req.Op, Path: req.Path, Recursive: req.Recursive}))

	sendKey, receiveKey, err := QuicClientKeyExchange(conn)
	require.NoError(t, err)

	require.NoError(t, WriteEncryptedMsg(conn, req, sendKey))
	return sendKey, receiveKey
}

func TestQUICHandleListSingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello world"), 0644))

	client := newQUICTestStream(t)
	_, receiveKey := sendReq(t, client, FSRequest{Op: FSOpList, Path: filePath})

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
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
	_, receiveKey := sendReq(t, client, FSRequest{Op: FSOpList, Path: srcDir, Recursive: true})

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)
	require.Len(t, resp.Entries, 2)
}

func TestQUICHandleListDirWithoutRecursiveFails(t *testing.T) {
	dir := t.TempDir()

	client := newQUICTestStream(t)
	_, receiveKey := sendReq(t, client, FSRequest{Op: FSOpList, Path: dir})

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Err, "is a directory")
}

func TestQUICHandleListNonExistent(t *testing.T) {
	client := newQUICTestStream(t)
	_, receiveKey := sendReq(t, client,
		FSRequest{Op: FSOpList, Path: filepath.Join(t.TempDir(), "nope")})

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleReadRange(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "data.bin")
	content := []byte("0123456789ABCDEF")
	require.NoError(t, os.WriteFile(filePath, content, 0644))

	client := newQUICTestStream(t)
	req := FSRequest{Op: FSOpRead, Path: filePath, RelPath: "data.bin", Offset: 3, Length: 5}
	_, receiveKey := sendReq(t, client, req)

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)

	var buf bytes.Buffer
	require.NoError(t, CopyDecrypted(&buf, client, 5, receiveKey, nil))
	assert.Equal(t, content[3:8], buf.Bytes())
}

func TestQUICHandleReadNonExistentFile(t *testing.T) {
	dir := t.TempDir()

	client := newQUICTestStream(t)
	req := FSRequest{Op: FSOpRead, Path: dir, RelPath: "missing.txt", Offset: 0, Length: 1}
	_, receiveKey := sendReq(t, client, req)

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleInitAndWrite(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	entries := []TransferManifestEntry{
		{RelPath: testFileName, Size: 10, Mode: 0644},
	}

	initClient := newQUICTestStream(t)
	_, initReceiveKey := sendReq(t, initClient, FSRequest{
		Op: FSOpInit, Path: dst, Entries: entries, TargetIsDirHint: true,
	})
	var initResp FSResponse
	require.NoError(t, ReadEncryptedMsg(initClient, &initResp, initReceiveKey))
	require.True(t, initResp.OK)

	fi, err := os.Stat(filepath.Join(dst, testFileName))
	require.NoError(t, err)
	assert.EqualValues(t, 10, fi.Size())

	writeClient := newQUICTestStream(t)
	writeReq := FSRequest{
		Op: FSOpWrite, Path: dst, RelPath: testFileName, Offset: 2, Length: 5, TargetIsDirHint: true,
	}
	sendKey, receiveKey := sendReq(t, writeClient, writeReq)

	var ack FSResponse
	require.NoError(t, ReadEncryptedMsg(writeClient, &ack, receiveKey))
	require.True(t, ack.OK)

	require.NoError(t, CopyEncrypted(writeClient, bytes.NewReader([]byte("HELLO")), 5, sendKey, nil))

	var final FSResponse
	require.NoError(t, ReadEncryptedMsg(writeClient, &final, receiveKey))
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
	req := FSRequest{
		Op: FSOpWrite, Path: dst, RelPath: testFileName, Offset: 0, Length: 5, TargetIsDirHint: true,
	}
	_, receiveKey := sendReq(t, client, req)

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestQUICHandleInitSingleFileTarget(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "renamed.txt")

	entries := []TransferManifestEntry{
		{RelPath: "original.txt", Size: 4, Mode: 0644},
	}

	client := newQUICTestStream(t)
	_, receiveKey := sendReq(t, client, FSRequest{Op: FSOpInit, Path: dst, Entries: entries})

	var resp FSResponse
	require.NoError(t, ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)

	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.False(t, fi.IsDir())
	assert.EqualValues(t, 4, fi.Size())
}
