package deskconn

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

const (
	waitTimeout = 10 * time.Second
	waitTick    = 20 * time.Millisecond
)

// p2pTestConnection adapts a bare offerer *webrtc.PeerConnection to
// P2PConnection, letting tests drive DownloadFilesP2P/UploadFilesP2P without a
// real WAMP handshake -- xconnwebrtc.WebRTCSession satisfies P2PConnection
// the same way in production.
type p2pTestConnection struct {
	pc *webrtc.PeerConnection
}

func (c *p2pTestConnection) OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error) {
	return c.pc.CreateDataChannel(label, options)
}

func (c *p2pTestConnection) Close() error {
	return c.pc.Close()
}

// newP2PTestConnection establishes one real, connected pair of pion
// PeerConnections over loopback (host ICE candidates only, no STUN/TURN
// needed) and wires the answerer's incoming data channels to
// (*Deskconn).HandleAuxDataChannel -- the exact same production entry point
// cmd/deskconnd/main.go registers via webRtcManager.OnDataChannel, not a
// lower-level handler reached only in tests. It returns the offerer side as
// a P2PConnection.
func newP2PTestConnection(t *testing.T) P2PConnection {
	t.Helper()

	offererPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	answererPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = offererPC.Close()
		_ = answererPC.Close()
	})

	// At least one data channel must exist before the offer to bootstrap the
	// SCTP association; every channel opened afterward (by openP2PChannel)
	// rides that same association with no renegotiation needed.
	_, err = offererPC.CreateDataChannel("bootstrap", nil)
	require.NoError(t, err)

	var d Deskconn
	answererPC.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == "bootstrap" {
			return
		}
		var once sync.Once
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			once.Do(func() {
				d.HandleAuxDataChannel("test-session", dc, msg.Data)
			})
		})
	})

	offer, err := offererPC.CreateOffer(nil)
	require.NoError(t, err)
	offerGatherComplete := webrtc.GatheringCompletePromise(offererPC)
	require.NoError(t, offererPC.SetLocalDescription(offer))
	<-offerGatherComplete

	require.NoError(t, answererPC.SetRemoteDescription(*offererPC.LocalDescription()))
	answer, err := answererPC.CreateAnswer(nil)
	require.NoError(t, err)
	answerGatherComplete := webrtc.GatheringCompletePromise(answererPC)
	require.NoError(t, answererPC.SetLocalDescription(answer))
	<-answerGatherComplete

	require.NoError(t, offererPC.SetRemoteDescription(*answererPC.LocalDescription()))

	require.Eventually(t, func() bool {
		return offererPC.ConnectionState() == webrtc.PeerConnectionStateConnected
	}, waitTimeout, waitTick, "peer connections never reached the connected state")

	return &p2pTestConnection{pc: offererPC}
}

// newP2PTestConnector returns a P2PConnector that establishes a fresh,
// independent PeerConnection pair (via newP2PTestConnection) each time it's
// called -- mirroring how DownloadFilesP2P/UploadFilesP2P give each parallel
// worker its own connection in production.
func newP2PTestConnector(t *testing.T) P2PConnector {
	t.Helper()
	return func() (P2PConnection, error) {
		return newP2PTestConnection(t), nil
	}
}

func TestDownloadFilesP2PSingleFile(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	content := bytes.Repeat([]byte("abcdefghij"), 1000) // 10000 bytes, spans multiple chunks worth of messages
	srcFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "hello.txt")
	require.NoError(t, DownloadFilesP2P(sess, connect, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDownloadFilesP2PLargeFileSpansMultipleChunks(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	content := make([]byte, parallelChunkSize*2+12345)
	for i := range content {
		content[i] = byte(i)
	}
	srcFile := filepath.Join(t.TempDir(), "big.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "big.bin")
	require.NoError(t, DownloadFilesP2P(sess, connect, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDownloadFilesP2PDirectory(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, DownloadFilesP2P(sess, connect, srcDir, dstDir, true, 0))

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
	connect := newP2PTestConnector(t)
	err := DownloadFilesP2P(sess, connect, "/nonexistent/path/that/does/not/exist", t.TempDir(), false, 0)
	require.Error(t, err)
}

func TestUploadFilesP2PSingleFile(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	content := bytes.Repeat([]byte("zyxwvuts"), 1500)
	srcFile := filepath.Join(t.TempDir(), "upload.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "upload.bin")
	require.NoError(t, UploadFilesP2P(sess, connect, srcFile, dstFile, false, 0))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestUploadFilesP2PDirectory(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	srcDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "subdir"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root content"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "subdir", "nested.txt"), []byte("nested content"), 0644))

	dstDir := t.TempDir()
	require.NoError(t, UploadFilesP2P(sess, connect, srcDir, dstDir, true, 0))

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
	connect := newP2PTestConnector(t)
	err := UploadFilesP2P(sess, connect, "/nonexistent/local/path", t.TempDir(), false, 0)
	require.Error(t, err)
}

func TestDownloadFilesP2PCustomWorkerCount(t *testing.T) {
	sess := newP2PTestConnection(t)
	connect := newP2PTestConnector(t)

	content := make([]byte, parallelChunkSize*3+777)
	for i := range content {
		content[i] = byte(i * 7)
	}
	srcFile := filepath.Join(t.TempDir(), "custom.bin")
	require.NoError(t, os.WriteFile(srcFile, content, 0644))

	dstFile := filepath.Join(t.TempDir(), "custom.bin")
	require.NoError(t, DownloadFilesP2P(sess, connect, srcFile, dstFile, false, 1))

	got, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	require.Equal(t, content, got)
}
