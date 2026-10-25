package deskconn_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-core/bindings/go"
	webrtc "github.com/xconnio/xconn-core/bindings/go/webrtc"
)

const (
	waitTimeout = 10 * time.Second
	waitTick    = 20 * time.Millisecond
)

// File-transfer tests use the same Rust-backed WAMP/WebRTC connection as production.
func newP2PTestConnection(t *testing.T) deskconn.ChannelOpener {
	t.Helper()
	return deskconn.P2PChannels(newP2PTestPeerConnection(t))
}

func newP2PTestPeerConnection(t *testing.T) deskconn.P2PChannelOpener {
	t.Helper()
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm("test", &xconn.RealmConfig{AutoDiscloseCaller: true, Meta: true,
		Roles: []xconn.RealmRole{{Name: "anonymous", Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix",
			AllowCall: true, AllowRegister: true, AllowSubscribe: true, AllowPublish: true}}}}}))
	signaling, err := xconn.ConnectInMemory(router, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = signaling.Close() })
	provider := webrtc.NewWebRTCHandler()
	var backend userd.Deskconn
	provider.OnDataChannel(func(_ string, channel *webrtc.DataChannel, first []byte) {
		if first != nil {
			backend.HandleFileStreamChannel("test-session", channel, first)
			return
		}
		var once sync.Once
		channel.OnMessage(func(message webrtc.DataChannelMessage) {
			once.Do(func() { backend.HandleFileStreamChannel("test-session", channel, message.Data) })
		})
	})
	require.NoError(t, provider.SetupContext(t.Context(), &webrtc.ProviderConfig{
		Session: signaling, Router: router, ProcedureHandleOffer: "offer",
		TopicHandleRemoteCandidates: "to-provider", TopicPublishLocalCandidate: "to-client",
	}))
	t.Cleanup(func() { _ = provider.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	clientSignal, err := xconn.ConnectInMemory(router, "test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSignal.Close() })
	session, err := webrtc.ConnectWAMPContext(ctx, &webrtc.ClientConfig{Session: clientSignal, Realm: "test",
		ProcedureWebRTCOffer: "offer", TopicAnswererOnCandidate: "to-provider", TopicOffererOnCandidate: "to-client"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close(); _ = session.Connection().Close() })
	address, err := session.Connection().SelectedRemoteAddress(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, address, "VPN routing needs the selected ICE peer address")
	return session
}

func TestDownloadFilesP2PSingleFile(t *testing.T) {
	sess := newP2PTestConnection(t)

	content := bytes.Repeat([]byte("abcdefghij"), 1000) // 10000 bytes, spans multiple chunks worth of messages
	srcFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, deskconn.DownloadFilesP2P(sess, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDownloadFilesP2PLargeFileSpansMultipleChunks(t *testing.T) {
	sess := newP2PTestConnection(t)

	content := make([]byte, deskconn.ParallelChunkSize*2+12345)
	for i := range content {
		content[i] = byte(i)
	}
	srcFile := filepath.Join(t.TempDir(), "big.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "big.bin")
	require.NoError(t, deskconn.DownloadFilesP2P(sess, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDownloadFilesP2PDirectory(t *testing.T) {
	sess := newP2PTestConnection(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, deskconn.DownloadFilesP2P(sess, srcDir, dstDir, true, 0))

	base := filepath.Base(srcDir)
	got, err := os.ReadFile(filepath.Join(dstDir, base, "root.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("root content"), got)

	got, err = os.ReadFile(filepath.Join(dstDir, base, "subdir", "nested.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("nested content"), got)
}

func TestDownloadFilesP2PNonExistentPath(t *testing.T) {
	sess := newP2PTestConnection(t)
	err := deskconn.DownloadFilesP2P(sess, "/nonexistent/path/that/does/not/exist", t.TempDir(), false, 0)
	require.Error(t, err)
}

func TestUploadFilesP2PSingleFile(t *testing.T) {
	sess := newP2PTestConnection(t)

	content := bytes.Repeat([]byte("zyxwvuts"), 1500)
	srcFile := filepath.Join(t.TempDir(), "upload.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "upload.bin")
	require.NoError(t, deskconn.UploadFilesP2P(sess, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestUploadFilesP2PDirectory(t *testing.T) {
	sess := newP2PTestConnection(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, deskconn.UploadFilesP2P(sess, srcDir, dstDir, true, 0))

	base := filepath.Base(srcDir)
	got, err := os.ReadFile(filepath.Join(dstDir, base, "root.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("root content"), got)

	got, err = os.ReadFile(filepath.Join(dstDir, base, "subdir", "nested.txt"))
	require.NoError(t, err)
	require.Equal(t, []byte("nested content"), got)
}

func TestUploadFilesP2PNonExistentLocalPath(t *testing.T) {
	sess := newP2PTestConnection(t)
	err := deskconn.UploadFilesP2P(sess, "/nonexistent/local/path", t.TempDir(), false, 0)
	require.Error(t, err)
}

func TestDownloadFilesP2PCustomWorkerCount(t *testing.T) {
	sess := newP2PTestConnection(t)

	content := make([]byte, deskconn.ParallelChunkSize*3+777)
	for i := range content {
		content[i] = byte(i * 7)
	}
	srcFile := filepath.Join(t.TempDir(), "custom.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "custom.bin")
	require.NoError(t, deskconn.DownloadFilesP2P(sess, srcFile, dstFile, false, 1))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}
