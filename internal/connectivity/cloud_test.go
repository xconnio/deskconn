package connectivity_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

const (
	deviceRealm = "device"
	userAuthID  = "alice"
)

type cloudPolicy struct{}

func (cloudPolicy) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (cloudPolicy) Authenticate(request auth.Request) (auth.Response, error) {
	return auth.NewResponse(request.AuthID(), "trusted", 0)
}

func TestCloudAttachmentLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cloudRouter, err := api.NewRouter(nil)
	require.NoError(t, err)
	defer cloudRouter.Close()
	for _, realm := range []string{deviceRealm, "cloud"} {
		require.NoError(t, cloudRouter.AddRealm(realm, &api.RealmConfig{AutoDiscloseCaller: true, Meta: true}))
	}
	listener, err := api.NewServer(cloudRouter, cloudPolicy{}, nil).ListenQUIC(ctx, "127.0.0.1:0", nil)
	require.NoError(t, err)
	defer listener.Close()
	service, err := api.ConnectInMemory(cloudRouter, "cloud")
	require.NoError(t, err)
	defer service.Close()
	require.NoError(t, service.Register("keys", func(_ context.Context, _ *api.Invocation) *api.Result {
		publication := service.Publish("add.machine").
			Args("carol", "key3", "member").
			Acknowledge(true).
			ExecuteContext(ctx)
		if publication.Err != nil {
			return &api.Result{Err: publication.Err}
		}
		return api.NewResult(
			[]any{map[string]any{"authid": userAuthID, "authorized_keys": []any{"key1"}, "authrole": "owner"}},
		)
	}).ExecuteContext(ctx).Err)
	local, socket, localSession, err := app.StartAppLayer("local", filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	defer local.Close()
	defer socket.Close()
	defer localSession.Close()
	require.NoError(t, localSession.Register("io.xconn.echo", func(_ context.Context, inv *api.Invocation) *api.Result {
		return api.NewResult(inv.Args()...)
	}).ExecuteContext(ctx).Err)
	bobPublic, bobPrivate, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	streamSocket := filepath.Join(t.TempDir(), "streams.sock")
	backend, err := net.Listen("unix", streamSocket)
	require.NoError(t, err)
	defer backend.Close()
	backendResults := make(chan error, 4)
	go func() {
		for {
			connection, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				header, err := app.ReadRelayHeader(connection)
				if err != nil {
					backendResults <- err
					return
				}
				switch header.Kind {
				case app.RelayKindStream:
					request := make([]byte, 7)
					_, err = io.ReadFull(connection, request)
					if err == nil && string(request) != "request" {
						err = fmt.Errorf("incorrect raw relay payload")
					}
					if err == nil {
						_, err = connection.Write([]byte("answer"))
					}
				case app.RelayKindWebRTC:
					var data []byte
					var text bool
					data, text, err = app.ReadRelayFrame(connection)
					if err == nil && (!text || string(data) != "hello") {
						err = fmt.Errorf("text relay metadata was lost")
					}
					if err == nil {
						err = app.WriteRelayFrame(connection, []byte("answer"), true)
					}
				default:
					err = fmt.Errorf("unknown relay kind")
				}
				backendResults <- err
			}()
		}
	}()
	directory := t.TempDir()
	public, private, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	credentials := app.Credentials{Realm: deviceRealm, AuthID: "edge", PublicKey: public, PrivateKey: private}
	data, err := json.Marshal(credentials)
	require.NoError(t, err)
	credentialsPath := filepath.Join(directory, "credentials.json")
	require.NoError(t, os.WriteFile(credentialsPath, data, 0600))
	principalsPath := filepath.Join(directory, "principals.json")
	runContext, stop := context.WithCancel(ctx)
	defer stop()
	finished := make(chan error, 1)
	config := &app.CloudConfig{
		OnError: func(err error) { t.Logf("cloud retry: %v", err) },
		Address: listener.Address, TLSConfig: &tls.Config{ServerName: "localhost", MinVersion: tls.VersionTLS13},
		RootCertificates: [][]byte{listener.Certificate}, Realm: "cloud", MachineID: "machine",
		CredentialsFile: credentialsPath, PrincipalsFile: principalsPath,
		ProcedureListKeys: "keys", TopicKeyAddedFormat: "add.%s", TopicKeyRemovedFormat: "remove.%s",
		ICEServers:        []api.WebRTCICEServer{{URLs: []string{"stun:127.0.0.1:9"}}},
		TopicDetachFormat: "detach.%s", RetryInterval: 10 * time.Millisecond,
	}
	go func() {
		application := &app.App{Session: localSession, Prefixes: []string{"io.xconn."}, StreamSocket: streamSocket}
		finished <- app.Run(runContext, application, config)
	}()
	accepted, _, err := listener.AcceptConnection(ctx)
	require.NoError(t, err)
	defer accepted.Close()
	require.Eventually(t, func() bool {
		principals, err := app.ReadPrincipalsFromFile(principalsPath)
		return err == nil && len(principals) == 2 && principals[0].AuthID == userAuthID
	}, 3*time.Second, time.Millisecond)
	caller, err := api.ConnectInMemory(cloudRouter, deviceRealm)
	require.NoError(t, err)
	defer caller.Close()
	require.Eventually(t, func() bool {
		reply := caller.Call("io.xconn.echo").Arg("cloud to app").ExecuteContext(ctx)
		return reply.Err == nil && reply.ArgStringOr(0, "") == "cloud to app"
	}, 3*time.Second, time.Millisecond)
	require.NoError(
		t,
		service.Publish("add.machine").Args("bob", bobPublic, "member").Acknowledge(true).ExecuteContext(ctx).Err,
	)
	require.Eventually(t, func() bool {
		principals, err := app.ReadPrincipalsFromFile(principalsPath)
		return err == nil && len(principals) == 3
	}, time.Second, time.Millisecond)
	raw, err := accepted.OpenStream(ctx)
	require.NoError(t, err)
	_, err = raw.WriteContext(ctx, []byte("request"))
	require.NoError(t, err)
	rawReply := make([]byte, 6)
	_, err = io.ReadFull(raw, rawReply)
	require.NoError(t, err)
	require.Equal(t, "answer", string(rawReply))
	require.NoError(t, raw.Close())
	select {
	case err := <-backendResults:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("raw relay did not finish")
	}
	bobAuthenticator, err := auth.NewCryptoSignAuthenticator("bob", bobPrivate, nil)
	require.NoError(t, err)
	directConfig := &api.WebRTCClientConfig{
		Session: caller, Realm: deviceRealm, Authenticator: bobAuthenticator, Serializer: api.CBORSerializerSpec,
		ProcedureWebRTCOffer:     app.ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: app.TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  app.TopicOffererOnCandidate,
	}
	direct, err := api.ConnectWebRTCContext(ctx, directConfig)
	require.NoError(t, err)
	require.NoError(t, direct.Call("io.xconn.echo").Arg("direct").ExecuteContext(ctx).Err)
	channel, err := direct.OpenChannel("shell", nil)
	require.NoError(t, err)
	require.NoError(t, channel.SendText("hello"))
	packet, err := channel.ReadDataPacketContext(ctx)
	require.NoError(t, err)
	require.True(t, packet.IsString)
	require.Equal(t, "answer", string(packet.Data))
	require.NoError(t, channel.Close())
	select {
	case err := <-backendResults:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("WebRTC relay did not finish")
	}
	require.NoError(t, direct.Close())
	require.NoError(t, direct.Connection().Close())
	require.NoError(
		t,
		service.Publish("remove.machine").
			Arg(map[string]any{"bob": []any{bobPublic}}).
			Acknowledge(true).
			ExecuteContext(ctx).
			Err,
	)
	require.Eventually(t, func() bool {
		principals, err := app.ReadPrincipalsFromFile(principalsPath)
		return err == nil && len(principals) == 2 && principals[0].AuthID == userAuthID
	}, time.Second, time.Millisecond)
	revoked, err := api.ConnectWebRTCContext(ctx, directConfig)
	if revoked != nil {
		_ = revoked.Close()
		_ = revoked.Connection().Close()
	}
	require.ErrorContains(t, err, "authentication", "a removed key must not open a new direct session")
	require.NoError(t, accepted.Close())
	reconnected, _, err := listener.AcceptConnection(ctx)
	require.NoError(t, err)
	defer reconnected.Close()
	require.Eventually(t, func() bool {
		reply := caller.Call("io.xconn.echo").Arg("reconnected").ExecuteContext(ctx)
		return reply.Err == nil && reply.ArgStringOr(0, "") == "reconnected"
	}, 3*time.Second, time.Millisecond)
	require.NoError(t, service.Publish("detach.machine").Acknowledge(true).ExecuteContext(ctx).Err)
	require.Eventually(
		t,
		func() bool { _, err := os.Stat(credentialsPath); return os.IsNotExist(err) },
		3*time.Second,
		time.Millisecond,
	)
	require.NoError(t, os.WriteFile(credentialsPath, data, 0600))
	reattached, _, err := listener.AcceptConnection(ctx)
	require.NoError(t, err)
	defer reattached.Close()
	require.Eventually(t, func() bool {
		reply := caller.Call("io.xconn.echo").Arg("reattached").ExecuteContext(ctx)
		return reply.Err == nil && reply.ArgStringOr(0, "") == "reattached"
	}, 3*time.Second, time.Millisecond)
	cloudRouter.RemoveRealm(deviceRealm)
	require.Eventually(
		t,
		func() bool { _, err := os.Stat(credentialsPath); return os.IsNotExist(err) },
		3*time.Second,
		time.Millisecond,
	)
	stop()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("cloud runtime failed to stop")
	}
}
