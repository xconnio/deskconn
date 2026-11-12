package deskconn_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

func listenQUIC(t *testing.T, router *xconn.Router, authenticator auth.ServerAuthenticator) (string, string) {
	t.Helper()
	directory := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem"))
	require.NoError(t, err)
	server := xconn.NewServer(router, authenticator, nil)
	t.Cleanup(func() { _ = server.Close() })
	listener, err := server.ListenQUIC(t.Context(), "127.0.0.1:0", &cert)
	require.NoError(t, err)
	return "quic://" + listener.Address, common.CertificateFingerprint(cert)
}

const streamProxyTestRealm = "io.xconn.test"

type anyKeyAuthenticator struct{}

func (anyKeyAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (anyKeyAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("cryptosign only")
}

func startStreamProxy(t *testing.T) (string, *userd.ClientSessions) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rpcproxy")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm(streamProxyTestRealm, &xconn.RealmConfig{Roles: []xconn.RealmRole{{
		Name: "owner", Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix", AllowCall: true}},
	}}}))
	host, err := xconn.ConnectInMemory(router, streamProxyTestRealm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	backend := userd.NewDeskconn(false, t.TempDir())
	require.NoError(t, backend.Register(host))
	t.Cleanup(func() { backend.CloseStreams() })
	machineURL, certHash := listenQUIC(t, router, anyKeyAuthenticator{})
	_, key, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	authenticator, err := auth.NewCryptoSignAuthenticator("client", key, nil)
	require.NoError(t, err)
	connection, err := common.ConnectStandalone(t.Context(), machineURL, certHash, streamProxyTestRealm, authenticator)
	require.NoError(t, err)
	clients := userd.NewClientSessions()
	lifetime, cancel := context.WithCancel(t.Context())
	clients.StoreMachineSession(streamProxyTestRealm, connection.Session, nil, connection, lifetime, cancel)
	t.Cleanup(clients.DisconnectAll)
	local, listener, session, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(dir, "deskconn.sock"))
	require.NoError(t, err)
	t.Cleanup(local.Close)
	t.Cleanup(func() { _ = listener.Close(); _ = session.Close() })
	registration := session.Register(common.ProcedureProxyStream, userd.ProxyStreamHandler(clients, dir))
	require.NoError(t, registration.ExecuteContext(t.Context()).Err)
	return dir, clients
}

func TestDaemonStreamsTransferFiles(t *testing.T) {
	cfgDirectory, _ := startStreamProxy(t)

	daemon, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	require.False(t, daemon.P2P(), "a QUIC-only persistent connection must be reported as such")

	content := make([]byte, 3*1024*1024+123)
	for i := range content {
		content[i] = byte(i)
	}
	src := filepath.Join(t.TempDir(), "src.bin")
	require.NoError(t, os.WriteFile(src, content, 0600))

	uploaded := filepath.Join(t.TempDir(), "uploaded.bin")
	require.NoError(t, deskconn.UploadFiles(daemon, src, uploaded, false, 0))
	got, err := os.ReadFile(uploaded)
	require.NoError(t, err)
	require.Equal(t, content, got)

	downloaded := filepath.Join(t.TempDir(), "downloaded.bin")
	require.NoError(t, deskconn.DownloadFiles(daemon, uploaded, downloaded, false, 0))
	got, err = os.ReadFile(downloaded)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

func TestDaemonStreamsReusesPersistentConnection(t *testing.T) {
	cfgDirectory, clientSessions := startStreamProxy(t)
	before := clientSessions.MachineSessions()

	daemon, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0600))
	require.NoError(t, deskconn.UploadFiles(daemon, src, filepath.Join(t.TempDir(), "b.txt"), false, 0))

	require.Equal(t, before, clientSessions.MachineSessions(), "the proxy must not replace the persistent connection")
}

func TestDaemonStreamProxyRejectsNonStreamProcedures(t *testing.T) {
	directory, _ := startStreamProxy(t)
	daemon, err := deskconn.DialDaemonStreams(t.Context(), streamProxyTestRealm, directory)
	require.NoError(t, err)
	defer daemon.Close()
	result := daemon.Call(common.ProcedureKeyExchange).ExecuteContext(t.Context())
	var application *xconn.Error
	require.ErrorAs(t, result.Err, &application)
	require.Equal(t, common.ErrNotAuthorized, application.URI)
}

func TestDaemonStreamsDaemonNotRunning(t *testing.T) {
	_, err := deskconn.DialDaemonStreams(context.Background(), streamProxyTestRealm, t.TempDir())
	require.ErrorIs(t, err, deskconn.ErrDaemonUnavailable)
}
