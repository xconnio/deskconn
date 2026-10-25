package connectivity_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/serializers"
)

func TestBridgeIdentityPayloadAndCleanup(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "app.sock")
	local, listener, localSession, err := app.StartAppLayer("local", socketPath)
	require.NoError(t, err)
	defer local.Close()
	defer listener.Close()
	defer localSession.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend, err := (&api.Client{Serializer: "msgpack"}).Connect(ctx, "unix://"+socketPath, "local")
	require.NoError(t, err)
	defer backend.Close()

	remote, err := api.NewRouter(nil)
	require.NoError(t, err)
	defer remote.Close()
	require.NoError(t, remote.AddRealm("remote", &api.RealmConfig{AutoDiscloseCaller: true, Meta: true}))
	gateway, err := api.ConnectInMemory(remote, "remote")
	require.NoError(t, err)
	defer gateway.Close()
	created := make(chan *api.Session, 2)
	factory := func(id, role string) (*api.Session, error) {
		codec := &serializers.MsgPackSerializer{}
		base, err := api.ConnectInMemoryBase(local, "local", id, role, codec, 16)
		if err != nil {
			return nil, err
		}
		session, err := api.NewSession(base, base.Serializer())
		if err != nil {
			return nil, err
		}
		created <- session
		return session, nil
	}
	require.NoError(t, backend.Register("test.echo", func(_ context.Context, inv *api.Invocation) *api.Result {
		return &api.Result{Args: []any{inv.CallerAuthID(), inv.CallerAuthRole(), inv.Args()}, Kwargs: inv.Kwargs()}
	}).Execute().Err)
	require.NoError(t, backend.Register("test.fail", func(_ context.Context, _ *api.Invocation) *api.Result {
		return &api.Result{
			Err: &api.Error{URI: "test.failure", Args: []any{"reason"}, Kwargs: map[string]any{"code": int64(7)}},
		}
	}).Execute().Err)
	bridge, err := app.NewBridge(
		ctx,
		gateway,
		&app.App{Session: localSession, NewSession: factory, Prefixes: []string{"test."}},
	)
	require.NoError(t, err)
	defer bridge.Close()
	codec := &serializers.MsgPackSerializer{}
	base, err := api.ConnectInMemoryBase(remote, "remote", "alice", "trusted", codec, 16)
	require.NoError(t, err)
	caller, err := api.NewSession(base, base.Serializer())
	require.NoError(t, err)
	defer caller.Close()
	result := caller.Call("test.echo").Arg([]byte{0, 255}).Kwarg("value", "ok").ExecuteContext(ctx)
	require.NoError(t, result.Err)
	require.Equal(t, "alice", result.ArgStringOr(0, ""))
	require.Equal(t, "trusted", result.ArgStringOr(1, ""))
	require.Equal(t, "ok", result.Kwargs["value"])
	arguments, err := result.ArgList(2)
	require.NoError(t, err)
	binary, err := arguments.Bytes(0)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 255}, binary)

	var forwarded *api.Session
	select {
	case forwarded = <-created:
	case <-ctx.Done():
		t.Fatal("caller session not created")
	}
	result = caller.Call("test.fail").ExecuteContext(ctx)
	var failure *api.Error
	require.ErrorAs(t, result.Err, &failure)
	require.Equal(t, "test.failure", failure.URI)
	require.Equal(t, []any{"reason"}, failure.Args)
	require.EqualValues(t, 7, failure.Kwargs["code"])
	started := make(chan struct{})
	interrupted := make(chan struct{})
	require.NoError(t, backend.Register("test.wait", func(ctx context.Context, _ *api.Invocation) *api.Result {
		close(started)
		<-ctx.Done()
		close(interrupted)
		return api.NewResult()
	}).ExecuteContext(ctx).Err)
	callContext, cancelCall := context.WithCancel(ctx)
	completed := make(chan *api.Result, 1)
	go func() { completed <- caller.Call("test.wait").ExecuteContext(callContext) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("forwarded invocation did not start")
	}
	cancelCall()
	select {
	case result := <-completed:
		require.ErrorIs(t, result.Err, context.Canceled)
	case <-ctx.Done():
		t.Fatal("forwarded call did not cancel")
	}
	select {
	case <-interrupted:
	case <-ctx.Done():
		t.Fatal("local handler did not observe cancellation")
	}
	require.NoError(t, caller.Close())
	select {
	case <-forwarded.Done():
	case <-ctx.Done():
		t.Fatal("remote caller leave did not close local session")
	}
	require.NoError(t, bridge.Close())
}
