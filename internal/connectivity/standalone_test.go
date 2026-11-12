package connectivity_test

import (
	"bytes"
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
	"github.com/xconnio/xconn-core/bindings/go/serializers"
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
	socketPath := filepath.Join(t.TempDir(), "app.sock")
	local, listener, backend, err := app.StartAppLayer("local", socketPath)
	require.NoError(t, err)
	defer local.Close()
	defer listener.Close()
	defer backend.Close()
	application, err := app.ConnectApp(ctx, socketPath, "local")
	require.NoError(t, err)
	defer application.Close()
	require.NoError(t, application.Register("io.xconn.transfer", func(_ context.Context, inv *api.Invocation) *api.Result {
		stream, openErr := inv.ArgStream(1)
		if openErr != nil {
			return api.NewResultError("test.invalid_stream", openErr.Error())
		}
		defer stream.Close()
		body, readErr := io.ReadAll(stream)
		if readErr != nil {
			return api.NewResultError("test.read_failed", readErr.Error())
		}
		return api.NewResult(inv.CallerAuthID(), api.ReadStream(io.NopCloser(bytes.NewReader(body))))
	}).ExecuteContext(ctx).Err)
	require.NoError(t, application.Register("io.xconn.echo", func(_ context.Context, inv *api.Invocation) *api.Result {
		return api.NewResult(inv.Args()...)
	}).ExecuteContext(ctx).Err)
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
			Session: backend, Procedures: []string{"io.xconn.echo", "io.xconn.transfer"},
			NewSession: func(id, role string) (*api.Session, error) {
				//nolint:contextcheck // BaseSession constructors have no context API.
				base, joinErr := api.ConnectInMemoryBase(local, "local", id, role, &serializers.CBORSerializer{}, 16)
				if joinErr != nil {
					return nil, joinErr
				}
				//nolint:contextcheck // NewSession transfers the authenticated peer.
				session, joinErr := api.NewSession(base, base.Serializer())
				if joinErr != nil {
					_ = base.Close()
				}
				return session, joinErr
			},
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
	rtc, err := api.ConnectWebRTCContext(ctx, &api.WebRTCClientConfig{
		Session: quic.Session, Realm: standaloneTestRealm, Serializer: api.CBORSerializerSpec,
		Authenticator:        auth.NewWAMPCRAAuthenticator("carol", "password", nil),
		ProcedureWebRTCOffer: app.ProcedureWebRTCOffer, TopicAnswererOnCandidate: app.TopicAnswererOnCandidate,
		TopicOffererOnCandidate: app.TopicOffererOnCandidate,
	})
	require.NoError(t, err)
	defer rtc.Connection().Close()
	defer rtc.Close()
	content := bytes.Repeat([]byte{0, 255, 3, 128}, 384*1024)
	for _, session := range []*api.Session{quic.Session, wt.Session, rtc.Session} {
		result := session.Call("io.xconn.transfer").Arg("report.bin").
			Arg(api.ReadStream(io.NopCloser(bytes.NewReader(content)))).ExecuteContext(ctx)
		require.NoError(t, result.Err)
		require.Equal(t, "carol", result.ArgStringOr(0, ""))
		stream, openErr := result.ArgStream(1)
		require.NoError(t, openErr)
		body, readErr := io.ReadAll(stream)
		_ = stream.Close()
		require.NoError(t, readErr)
		require.Equal(t, content, body)
	}
	stop()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("standalone shutdown did not join its relay workers")
	}
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
