package userd_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

const proxyTestRealm = "io.xconn.test"

type cryptosignAuthenticator struct{}

func (cryptosignAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (cryptosignAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("cryptosign only")
}

func startEchoProxy(t *testing.T) (string, *userd.ClientSessions, *xconn.Session) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rpcproxy")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm(proxyTestRealm, &xconn.RealmConfig{Roles: []xconn.RealmRole{{
		Name: "owner", Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix", AllowCall: true}},
	}}}))
	host, err := xconn.ConnectInMemory(router, proxyTestRealm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	handler := func(ctx context.Context, _ *xconn.Invocation) *xconn.Result {
		endpoint, worker := net.Pipe()
		stop := context.AfterFunc(ctx, func() { _ = worker.Close() })
		go func() { defer stop(); defer worker.Close(); _, _ = io.Copy(worker, worker) }()
		return xconn.NewResult(nil, xconn.DuplexStream(endpoint))
	}
	require.NoError(t, host.Register(common.ProcedureShellStream, handler).ExecuteContext(t.Context()).Err)
	certificate, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	server := xconn.NewServer(router, cryptosignAuthenticator{}, nil)
	t.Cleanup(func() { _ = server.Close() })
	listener, err := server.ListenQUIC(t.Context(), "127.0.0.1:0", &certificate)
	require.NoError(t, err)
	_, privateKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	authenticator, err := auth.NewCryptoSignAuthenticator("client", privateKey, nil)
	require.NoError(t, err)
	connection, err := common.ConnectStandalone(t.Context(), "quic://"+listener.Address,
		common.CertificateFingerprint(certificate), proxyTestRealm, authenticator)
	require.NoError(t, err)
	clients := userd.NewClientSessions()
	lifetime, cancel := context.WithCancel(t.Context())
	clients.StoreMachineSession(proxyTestRealm, connection.Session, nil, connection, lifetime, cancel)
	t.Cleanup(clients.DisconnectAll)
	socket := filepath.Join(dir, "deskconn.sock")
	local, localListener, session, err := connectivity.StartAppLayer(common.LocalRealm, socket)
	require.NoError(t, err)
	t.Cleanup(local.Close)
	t.Cleanup(func() { _ = localListener.Close(); _ = session.Close() })
	require.NoError(t, session.Register(common.ProcedureProxyStream, userd.ProxyStreamHandler(clients, dir)).
		ExecuteContext(t.Context()).Err)
	return dir, clients, connection.Session
}

func requireStreamEcho(t *testing.T, stream net.Conn) {
	t.Helper()
	require.NoError(t, stream.SetDeadline(time.Now().Add(2*time.Second)))
	_, err := stream.Write([]byte("ping"))
	require.NoError(t, err)
	body := make([]byte, 4)
	_, err = io.ReadFull(stream, body)
	require.NoError(t, err)
	require.Equal(t, "ping", string(body))
}

func TestStreamProxyHoldsQUICForRunningOperation(t *testing.T) {
	directory, clients, machine := startEchoProxy(t)
	daemon, err := deskconn.DialDaemonStreams(t.Context(), proxyTestRealm, directory)
	require.NoError(t, err)
	defer daemon.Close()
	stream, err := common.OpenRPCStream(t.Context(), daemon, common.ProcedureShellStream, false)
	require.NoError(t, err)
	defer stream.Close()
	requireStreamEcho(t, stream)
	clients.RetireQUIC(proxyTestRealm)
	requireStreamEcho(t, stream)
	require.True(t, machine.Connected())
	require.NoError(t, stream.Close())
	select {
	case <-machine.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("retired connection survived its final stream")
	}
}
