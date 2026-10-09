package deskconnd_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

// listenQUIC serves router over QUIC on a local port with a self-signed certificate,
// returning the URL and certificate fingerprint to connect with and the raw streams.
func listenQUIC(t *testing.T, router *xconn.Router, authenticator auth.ServerAuthenticator) (string, string,
	<-chan *xconn.QUICStream) {
	t.Helper()
	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	listener, err := xconn.NewServer(router, authenticator, &xconn.ServerConfig{}).ListenAndServeQUIC("127.0.0.1:0",
		&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for range listener.AcceptSession() { //nolint:revive // only draining
		}
	}()
	return "quic://" + listener.Addr().String(), common.CertificateFingerprint(cert), listener.AcceptStream()
}

const proxyTestRealm = "io.xconn.test"

type cryptosignAuthenticator struct{}

func (cryptosignAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (cryptosignAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("cryptosign only")
}

// startEchoProxy serves the stream proxy in front of a persistent QUIC connection to a
// device that echoes every raw stream.
func startEchoProxy(t *testing.T) (cfgDirectory string, clientSessions *deskconnd.ClientSessions) {
	t.Helper()
	// Short prefix, not t.TempDir(): unix socket paths are length-limited, and t.TempDir()
	// embeds the full (often long) test name.
	dir, err := os.MkdirTemp("", "streamproxy")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	require.NoError(t, err)
	require.NoError(t, router.AddRealm(proxyTestRealm, &xconn.RealmConfig{Roles: []xconn.RealmRole{{
		Name:        "owner",
		Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix", AllowCall: true}},
	}}}))
	t.Cleanup(router.Close)
	deviceURL, certHash, streams := listenQUIC(t, router, cryptosignAuthenticator{})
	common.SafeGo(func() {
		for stream := range streams {
			common.SafeGo(func() { _, _ = io.Copy(stream, stream) })
		}
	})

	_, privateKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	authenticator, err := auth.NewCryptoSignAuthenticator("client", privateKey, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := common.ConnectStandalone(ctx, deviceURL, certHash, proxyTestRealm, authenticator)
	require.NoError(t, err)

	clientSessions = deskconnd.NewClientSessions()
	sessCtx, sessCancel := context.WithCancel(context.Background())
	clientSessions.StoreDeviceSession(proxyTestRealm, conn.Session, nil, conn, sessCtx, sessCancel)
	t.Cleanup(clientSessions.DisconnectAll)

	proxyListener, err := net.Listen("unix", filepath.Join(dir, common.StreamProxySocket))
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxyListener.Close() })
	common.SafeGo(func() { deskconnd.ServeStreamProxy(proxyListener, clientSessions, dir) })
	return dir, clientSessions
}

func requireEcho(t *testing.T, daemon *deskconn.DaemonStreams) {
	t.Helper()
	stream, err := daemon.OpenStream()
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Write([]byte("ping"))
	require.NoError(t, err)
	reply := make([]byte, 4)
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(stream, reply)
	require.NoError(t, err)
	require.Equal(t, "ping", string(reply))
}

// An operation that was given QUIC keeps it across the P2P upgrade (which retires the
// connection); once the operation is done, QUIC is gone.
func TestStreamProxyHoldsQUICForRunningOperation(t *testing.T) {
	cfgDirectory, clientSessions := startEchoProxy(t)

	daemon, err := deskconn.DialDaemonStreams(context.Background(), proxyTestRealm, cfgDirectory)
	require.NoError(t, err)
	require.False(t, daemon.P2P())
	requireEcho(t, daemon)

	clientSessions.RetireQUIC(proxyTestRealm)
	requireEcho(t, daemon)

	require.NoError(t, daemon.Close())
	// The proxy notices the closed hold asynchronously.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		stream, err := daemon.OpenStream()
		if err == nil {
			_ = stream.Close()
		}
		assert.Error(c, err)
	}, 2*time.Second, 10*time.Millisecond, "QUIC must close once nothing that started on it is running")
}
