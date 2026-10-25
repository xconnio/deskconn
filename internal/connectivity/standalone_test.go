package connectivity_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

const standaloneTestRealm = "standalone-test"

func unusedUDPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.LocalAddr().String()
	require.NoError(t, listener.Close())
	return address
}

func TestStandaloneServesApplicationAndStopsRelays(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local, listener, backend, err := app.StartAppLayer("local", filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	defer local.Close()
	defer listener.Close()
	defer backend.Close()
	require.NoError(t, backend.Register("io.xconn.echo", func(_ context.Context, inv *api.Invocation) *api.Result {
		return api.NewResult(inv.Args()...)
	}).ExecuteContext(ctx).Err)
	relay, err := net.Listen("unix", filepath.Join(t.TempDir(), "relay.sock"))
	require.NoError(t, err)
	defer relay.Close()
	identity, err := api.GenerateSelfSignedTLSConfig()
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(identity.Certificates[0].Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	trust := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	quicAddress, wtAddress := unusedUDPAddress(t), unusedUDPAddress(t)
	stopped := make(chan error, 1)
	serving, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		stopped <- app.RunStandalone(serving, &app.App{
			Session: backend, Procedures: []string{"io.xconn.echo"}, StreamSocket: relay.Addr().String(),
		},
			&app.StandaloneConfig{QUICAddress: quicAddress, WebTransportAddress: wtAddress, TLSConfig: identity,
				Realm: standaloneTestRealm, Passwords: map[string]string{"carol": "password"}})
	}()
	var quic *api.QUICSession
	require.Eventually(t, func() bool {
		attempt, release := context.WithTimeout(ctx, 100*time.Millisecond)
		defer release()
		quic, err = api.ConnectQUIC(attempt, quicAddress, standaloneTestRealm, &api.QUICDialerConfig{
			TLSConfig: trust, Authenticator: auth.NewWAMPCRAAuthenticator("carol", "password", nil)})
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)
	defer quic.Connection().Close()
	defer quic.Close()
	wt, err := api.ConnectWebTransport(
		ctx,
		"https://"+wtAddress+app.WebTransportPath,
		standaloneTestRealm,
		&api.WebTransportDialerConfig{
			TLSClientConfig: trust, Authenticator: auth.NewWAMPCRAAuthenticator("carol", "password", nil)},
	)
	require.NoError(t, err)
	defer wt.Connection().Close()
	defer wt.Close()
	for _, session := range []*api.Session{quic.Session, wt.Session} {
		require.Eventually(t, func() bool {
			response := session.Call("io.xconn.echo").Arg("hello").ExecuteContext(ctx)
			return response.Err == nil && response.ArgStringOr(0, "") == "hello"
		}, time.Second, 10*time.Millisecond)
	}
	stream, err := wt.OpenStream()
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Write([]byte("payload"))
	require.NoError(t, err)
	unix, err := relay.Accept()
	require.NoError(t, err)
	defer unix.Close()
	require.NoError(t, unix.SetDeadline(time.Now().Add(time.Second)))
	header, err := app.ReadRelayHeader(unix)
	require.NoError(t, err)
	require.Equal(t, app.RelayKindStream, header.Kind)
	payload := make([]byte, 7)
	_, err = io.ReadFull(unix, payload)
	require.NoError(t, err)
	require.Equal(t, "payload", string(payload))
	stop()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("standalone shutdown did not join its relay workers")
	}
	_, err = unix.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func TestStandaloneCustomAuthenticatorOverWebSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local, listener, backend, err := app.StartAppLayer("local", filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	defer local.Close()
	defer listener.Close()
	defer backend.Close()
	identity, err := api.GenerateSelfSignedTLSConfig()
	require.NoError(t, err)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reserved.Addr().String()
	require.NoError(t, reserved.Close())
	serving, stop := context.WithCancel(ctx)
	quicAddress := unusedUDPAddress(t)
	stopped := make(chan error, 1)
	go func() {
		stopped <- app.RunStandalone(serving, &app.App{Session: backend}, &app.StandaloneConfig{
			Realm: standaloneTestRealm, QUICAddress: quicAddress, WebSocketAddress: address, TLSConfig: identity,
			Authenticator: app.NewStandaloneAuthenticator(nil, map[string]string{"carol": "password"}),
		})
	}()
	defer func() {
		stop()
		select {
		case err := <-stopped:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("standalone WebSocket listener did not stop")
		}
	}()
	client := &api.Client{Authenticator: auth.NewWAMPCRAAuthenticator("carol", "password", nil)}
	var session *api.Session
	require.Eventually(t, func() bool {
		attempt, release := context.WithTimeout(ctx, 100*time.Millisecond)
		defer release()
		session, err = client.Connect(attempt, "ws://"+address, standaloneTestRealm)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond)
	defer session.Close()
	require.Equal(t, "carol", session.Details().AuthID())
	require.Equal(t, "owner", session.Details().AuthRole())
}
