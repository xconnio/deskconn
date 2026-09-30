package deskconn_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/deskconn/xlink"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const streamProxyTestRealm = "io.xconn.test"

type anyKeyAuthenticator struct{}

func (anyKeyAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (anyKeyAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("cryptosign only")
}

// startStreamProxy stands up the whole default-mode path short of the cloud: a
// device serving raw streams over yamux through xlink's relay to deskconnd's real
// handlers, a ClientSessions holding a persistent connection to it, and deskconnd's
// stream proxy in front of that. It returns the config directory to dial the proxy in.
func startStreamProxy(t *testing.T) (cfgDirectory string, clientSessions *deskconnd.ClientSessions) {
	t.Helper()
	dir := t.TempDir()

	// Device: deskconnd's relay listener, fed by xlink's QUIC-stream relay.
	relaySock := filepath.Join(dir, "xlink-streams.sock")
	relayListener, err := net.Listen("unix", relaySock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = relayListener.Close() })
	device := deskconnd.NewDeskconn(nil, nil, nil, false, dir)
	common.SafeGo(func() { device.ServeStreamRelay(relayListener) })

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	require.NoError(t, err)
	require.NoError(t, router.AddRealm(streamProxyTestRealm, &xconn.RealmConfig{Roles: []xconn.RealmRole{{
		Name:        "owner",
		Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix", AllowCall: true}},
	}}}))
	t.Cleanup(router.Close)
	listener, err := common.ListenYamux("unix://"+filepath.Join(dir, "device.sock"), router, anyKeyAuthenticator{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	common.SafeGo(func() {
		for stream := range listener.Streams() {
			common.SafeGo(func() { xlink.RelayQUICStream(stream.Conn, relaySock) })
		}
	})

	// Client: the persistent connection, and the stream proxy in front of it.
	_, privateKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	authenticator, err := auth.NewCryptoSignAuthenticator("client", privateKey, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := common.ConnectYamux(ctx, "unix://"+filepath.Join(dir, "device.sock"), streamProxyTestRealm,
		authenticator)
	require.NoError(t, err)

	clientSessions = deskconnd.NewClientSessions()
	sessCtx, sessCancel := context.WithCancel(context.Background())
	clientSessions.StoreDeviceSession(streamProxyTestRealm, conn.Session, nil, conn, sessCtx, sessCancel)
	t.Cleanup(clientSessions.DisconnectAll)

	proxyListener, err := net.Listen("unix", filepath.Join(dir, common.StreamProxySocket))
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxyListener.Close() })
	common.SafeGo(func() { deskconnd.ServeStreamProxy(proxyListener, clientSessions, dir) })

	return dir, clientSessions
}

func TestDaemonStreamsTransferFiles(t *testing.T) {
	cfgDirectory, _ := startStreamProxy(t)

	daemon, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	require.False(t, daemon.P2P(), "a QUIC-only persistent connection must be reported as such")

	content := make([]byte, 3*common.FileStreamChunkSize+123)
	for i := range content {
		content[i] = byte(i)
	}
	src := filepath.Join(t.TempDir(), "src.bin")
	require.NoError(t, os.WriteFile(src, content, 0600))

	uploaded := filepath.Join(t.TempDir(), "uploaded.bin")
	require.NoError(t, deskconn.UploadFilesDaemon(daemon, src, uploaded, false, 0))
	got, err := os.ReadFile(uploaded)
	require.NoError(t, err)
	require.Equal(t, content, got)

	downloaded := filepath.Join(t.TempDir(), "downloaded.bin")
	require.NoError(t, deskconn.DownloadFilesDaemon(daemon, uploaded, downloaded, false, 0))
	got, err = os.ReadFile(downloaded)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDaemonStreamsReusesPersistentConnection(t *testing.T) {
	cfgDirectory, clientSessions := startStreamProxy(t)
	before := clientSessions.DeviceSessions()

	daemon, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0600))
	require.NoError(t, deskconn.UploadFilesDaemon(daemon, src, filepath.Join(t.TempDir(), "b.txt"), false, 0))

	require.Equal(t, before, clientSessions.DeviceSessions(), "the proxy must not replace the persistent connection")
}

func TestDaemonStreamsP2PUnavailable(t *testing.T) {
	cfgDirectory, _ := startStreamProxy(t)

	daemon, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	_, err = daemon.OpenMessageChannel(common.LogChannelLabel)
	require.ErrorContains(t, err, "no longer P2P")
}

func TestDaemonStreamsDaemonNotRunning(t *testing.T) {
	_, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, t.TempDir())
	require.ErrorIs(t, err, deskconn.ErrDaemonUnavailable)
}

// relayedChannels opens each channel the way deskconnd's stream proxy does for a
// P2P persistent connection: the real data channel is spliced by
// common.RelayWebRTCChannel onto a local connection whose other end the client
// uses as a common.RelayChannel.
type relayedChannels struct{ p2p deskconn.P2PChannelOpener }

func (r relayedChannels) OpenMessageChannel(label string) (common.MessageChannel, error) {
	channel, err := common.OpenDataChannel(r.p2p, label)
	if err != nil {
		return nil, err
	}
	daemonSide, clientSide := net.Pipe()
	common.SafeGo(func() { common.RelayWebRTCChannel(channel, daemonSide, nil) })
	return common.NewRelayChannel(clientSide), nil
}

func TestTransferFilesOverRelayedChannels(t *testing.T) {
	sess := relayedChannels{p2p: newP2PTestPeerConnection(t)}

	content := make([]byte, 3*common.FileStreamChunkSize+123)
	for i := range content {
		content[i] = byte(i * 7)
	}
	src := filepath.Join(t.TempDir(), "src.bin")
	require.NoError(t, os.WriteFile(src, content, 0600))

	uploaded := filepath.Join(t.TempDir(), "uploaded.bin")
	require.NoError(t, deskconn.UploadFilesP2P(sess, src, uploaded, false, 0))
	downloaded := filepath.Join(t.TempDir(), "downloaded.bin")
	require.NoError(t, deskconn.DownloadFilesP2P(sess, uploaded, downloaded, false, 0))

	got, err := os.ReadFile(downloaded)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

// The P2P shell reader must return every message the proxy relayed before the channel
// closed: the relay reports the close right after the last message, and a reader that
// let the close win lost the tail of a command's output.
func TestShellReaderKeepsOutputBeforeClose(t *testing.T) {
	for range 200 {
		relaySide, cliSide := net.Pipe()
		go func() {
			for i := range 5 {
				_ = common.WriteRelayFrame(relaySide, []byte{byte(i)}, false)
			}
			_ = relaySide.Close()
		}()
		got := deskconn.ReadAllShellEnvelopes(common.NewRelayChannel(cliSide))
		require.Len(t, got, 5, "output relayed before the close was lost")
	}
}
