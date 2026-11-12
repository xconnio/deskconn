package userd_test

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

const testFileName = "file.txt"

func newRPCFileStream(t *testing.T, operation common.FSOp) net.Conn {
	t.Helper()
	caller, _ := newRPCFeatureSession(t)
	procedure, input := common.ProcedureFileListStream, false
	switch operation {
	case common.FSOpRead:
		procedure = common.ProcedureFileReadStream
	case common.FSOpInit:
		procedure, input = common.ProcedureFileInitStream, true
	case common.FSOpWrite:
		procedure, input = common.ProcedureFileWriteStream, true
	}
	stream, err := common.OpenRPCStream(t.Context(), caller, procedure, input)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// sendReq performs the key exchange and sends the encrypted request.
func sendReq(t *testing.T, conn net.Conn, req common.FSRequest) (sendKey, receiveKey []byte) {
	t.Helper()

	sendKey, receiveKey, err := deskconn.StreamClientKeyExchange(conn)
	require.NoError(t, err)

	require.NoError(t, common.WriteEncryptedMsg(conn, req, sendKey))
	return sendKey, receiveKey
}

func TestRPCHandleListSingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello world"), 0644))

	client := newRPCFileStream(t, common.FSOpList)
	_, receiveKey := sendReq(t, client, common.FSRequest{Op: common.FSOpList, Path: filePath})

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "hello.txt", resp.Entries[0].RelPath)
	assert.EqualValues(t, 11, resp.Entries[0].Size)
	assert.False(t, resp.Entries[0].IsDir)
}

func TestRPCHandleListDirRecursive(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0644))

	client := newRPCFileStream(t, common.FSOpList)
	_, receiveKey := sendReq(t, client, common.FSRequest{Op: common.FSOpList, Path: srcDir, Recursive: true})

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)
	require.Len(t, resp.Entries, 2)
}

func TestRPCHandleListDirWithoutRecursiveFails(t *testing.T) {
	dir := t.TempDir()

	client := newRPCFileStream(t, common.FSOpList)
	_, receiveKey := sendReq(t, client, common.FSRequest{Op: common.FSOpList, Path: dir})

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Err, "is a directory")
}

func TestRPCHandleListNonExistent(t *testing.T) {
	client := newRPCFileStream(t, common.FSOpList)
	_, receiveKey := sendReq(t, client,
		common.FSRequest{Op: common.FSOpList, Path: filepath.Join(t.TempDir(), "nope")})

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestRPCHandleReadRange(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "data.bin")
	content := []byte("0123456789ABCDEF")
	require.NoError(t, os.WriteFile(filePath, content, 0644))

	client := newRPCFileStream(t, common.FSOpRead)
	req := common.FSRequest{Op: common.FSOpRead, Path: filePath, RelPath: "data.bin", Offset: 3, Length: 5}
	_, receiveKey := sendReq(t, client, req)

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)

	var buf bytes.Buffer
	require.NoError(t, common.CopyDecrypted(&buf, client, 5, receiveKey, nil))
	assert.Equal(t, content[3:8], buf.Bytes())
}

func TestRPCHandleReadNonExistentFile(t *testing.T) {
	dir := t.TempDir()

	client := newRPCFileStream(t, common.FSOpRead)
	req := common.FSRequest{Op: common.FSOpRead, Path: dir, RelPath: "missing.txt", Offset: 0, Length: 1}
	_, receiveKey := sendReq(t, client, req)

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestRPCHandleInitAndWrite(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	entries := []common.TransferManifestEntry{
		{RelPath: testFileName, Size: 10, Mode: 0644},
	}

	initClient := newRPCFileStream(t, common.FSOpInit)
	_, initReceiveKey := sendReq(t, initClient, common.FSRequest{
		Op: common.FSOpInit, Path: dst, Entries: entries, TargetIsDirHint: true,
	})
	var initResp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(initClient, &initResp, initReceiveKey))
	require.True(t, initResp.OK)

	fi, err := os.Stat(filepath.Join(dst, testFileName))
	require.NoError(t, err)
	assert.EqualValues(t, 10, fi.Size())

	writeClient := newRPCFileStream(t, common.FSOpWrite)
	writeReq := common.FSRequest{
		Op: common.FSOpWrite, Path: dst, RelPath: testFileName, Offset: 2, Length: 5, TargetIsDirHint: true,
	}
	sendKey, receiveKey := sendReq(t, writeClient, writeReq)

	var ack common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(writeClient, &ack, receiveKey))
	require.True(t, ack.OK)

	require.NoError(t, common.CopyEncrypted(writeClient, bytes.NewReader([]byte("HELLO")), 5, sendKey, nil))

	var final common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(writeClient, &final, receiveKey))
	require.True(t, final.OK)

	got, err := os.ReadFile(filepath.Join(dst, testFileName))
	require.NoError(t, err)
	want := make([]byte, 10)
	copy(want[2:7], "HELLO")
	assert.Equal(t, want, got)
}

func TestRPCHandleWriteBeforeInitFails(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	client := newRPCFileStream(t, common.FSOpWrite)
	req := common.FSRequest{
		Op: common.FSOpWrite, Path: dst, RelPath: testFileName, Offset: 0, Length: 5, TargetIsDirHint: true,
	}
	_, receiveKey := sendReq(t, client, req)

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	assert.False(t, resp.OK)
	assert.NotEmpty(t, resp.Err)
}

func TestRPCHandleInitSingleFileTarget(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "renamed.txt")

	entries := []common.TransferManifestEntry{
		{RelPath: "original.txt", Size: 4, Mode: 0644},
	}

	client := newRPCFileStream(t, common.FSOpInit)
	_, receiveKey := sendReq(t, client, common.FSRequest{Op: common.FSOpInit, Path: dst, Entries: entries})

	var resp common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(client, &resp, receiveKey))
	require.True(t, resp.OK)

	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.False(t, fi.IsDir())
	assert.EqualValues(t, 4, fi.Size())
}

func TestRPCFileStreamCannotSwitchToAnotherOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist")
	stream := newRPCFileStream(t, common.FSOpList)
	_, receiveKey := sendReq(t, stream, common.FSRequest{Op: common.FSOpInit, Path: path})
	var response common.FSResponse
	require.NoError(t, common.ReadEncryptedMsg(stream, &response, receiveKey))
	require.False(t, response.OK)
	require.Equal(t, "operation does not match RPC endpoint", response.Err)
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))
}
