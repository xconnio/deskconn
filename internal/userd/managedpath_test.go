package userd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/protocol"
)

// procedureTestPath answers which path a call took to the test machine.
const procedureTestPath = "com.deskconn.test.path"

const (
	pathRouted = "routed"
	pathDirect = "direct"
)

// machineDirect makes the test machine reachable directly, as connectivity.Run does: a router
// of its own serving procedureTestPath, advertised on a session to the cloud with Expose. The
// machine stops being reachable directly when the returned func is called.
func (c *testCloud) machineDirect(ctx context.Context) (stop func()) {
	c.t.Helper()
	edge, err := xconn.NewRouter(nil)
	require.NoError(c.t, err)
	c.t.Cleanup(edge.Close)
	require.NoError(c.t, edge.AddRealm(cloudMachineRealm, &xconn.RealmConfig{
		AutoDiscloseCaller: true, Meta: true,
		Roles: []xconn.RealmRole{{Name: cloudUserRole, Permissions: []xconn.Permission{
			{URI: common.ProcedurePrefix, MatchPolicy: protocol.MatchPrefix, AllowCall: true},
		}}},
	}))
	backend, err := xconn.ConnectInMemory(edge, cloudMachineRealm)
	require.NoError(c.t, err)
	c.t.Cleanup(func() { _ = backend.Close() })
	c.serveMachine(backend)
	require.NoError(c.t, backend.Register(procedureTestPath, func(context.Context, *xconn.Invocation) *xconn.Result {
		return xconn.NewResult(pathDirect)
	}).ExecuteContext(ctx).Err)

	signaling, err := xconn.ConnectCryptosign(ctx, c.rawSocketURI, cloudMachineRealm, cloudMachineAuthID, c.machineKey)
	require.NoError(c.t, err)
	c.t.Cleanup(func() { _ = signaling.Close() })
	endpoint, err := signaling.Expose(ctx, edge, &xconn.ExposeConfig{
		Authenticator: c.authenticator, ICEServers: []xconn.WebRTCICEServer{},
	})
	require.NoError(c.t, err)
	c.t.Cleanup(func() { _ = endpoint.Close() })
	return func() { require.NoError(c.t, endpoint.Close()) }
}

// The daemon's connection to a machine starts on the cloud, goes direct once the machine
// advertises a direct path, falls back to the same cloud session when that path goes, and is
// made again only when the cloud session itself goes.
func TestMachineSessionFollowsTheDirectPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cloud := startTestCloud(t)
	cloud.machineOnline()
	routed, err := xconn.ConnectCryptosign(ctx, cloud.rawSocketURI, cloudMachineRealm, cloudMachineAuthID,
		cloud.machineKey)
	require.NoError(t, err)
	t.Cleanup(func() { _ = routed.Close() })
	require.NoError(t, routed.Register(procedureTestPath, func(context.Context, *xconn.Invocation) *xconn.Result {
		return xconn.NewResult(pathRouted)
	}).ExecuteContext(ctx).Err)

	sessions := userd.NewClientSessions()
	sessions.Login()
	t.Cleanup(sessions.DisconnectAll)
	session, err := sessions.EnsureMachineSession(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	path := func() string {
		return session.Call(procedureTestPath).ExecuteContext(ctx).ArgStringOr(0, "")
	}
	// Encrypted calls the CLI makes through the daemon: the machine keeps their keys by caller,
	// whom each path is a different one of.
	local, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(local.Close)
	require.NoError(t, local.AddRealm(common.LocalRealm, xconn.DefaultRealmConfig()))
	daemon, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)
	require.NoError(t, daemon.Register(common.ProcedureProxyFileOp,
		userd.ProxyFileOpHandler(sessions, cloud.cfgDirectory)).ExecuteContext(ctx).Err)
	cli, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)
	requireEcho := func() {
		t.Helper()
		resp := cli.Call(common.ProcedureProxyFileOp).Args(cloudMachineRealm, procedureTestEcho, []byte("hi")).
			ExecuteContext(ctx)
		require.NoError(t, resp.Err)
		require.Equal(t, "hi", string(resp.ArgBytesOr(0, nil)))
	}
	require.Equal(t, pathRouted, path(), "calls don't wait for a direct path")
	requireEcho()
	direct, err := sessions.DirectPath(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	require.False(t, direct)

	stopDirect := cloud.machineDirect(ctx)
	require.Eventually(t, func() bool { return session.Path() == xconn.PathDirect }, 30*time.Second,
		50*time.Millisecond, "the direct path is negotiated in the background")
	require.Equal(t, pathDirect, path(), "new calls take it")
	requireEcho()
	direct, err = sessions.DirectPath(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	require.True(t, direct, "what the CLI asks the daemon")

	stopDirect()
	require.Eventually(t, func() bool { return session.Path() == xconn.PathRouted }, 10*time.Second,
		50*time.Millisecond)
	require.Equal(t, pathRouted, path(), "new calls go back to the cloud")
	requireEcho()
	again, err := sessions.EnsureMachineSession(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	require.Same(t, session, again, "on the same session, not one made again")

	require.NoError(t, session.Close())
	require.Eventually(t, func() bool {
		next, ok := sessions.SessionByRealm(cloudMachineRealm)
		return ok && next != session && next.Connected()
	}, 20*time.Second, 50*time.Millisecond, "losing the cloud session reconnects")
	next, _ := sessions.SessionByRealm(cloudMachineRealm)
	require.Equal(t, pathRouted, next.Call(procedureTestPath).ExecuteContext(ctx).ArgStringOr(0, ""))
}

// --mode routed, and the signaling of --mode direct and VPN, stay on the cloud connection
// even to a machine reachable directly.
func TestRoutedConnectionStaysRouted(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cloud := startTestCloud(t)
	cloud.machineOnline()
	cloud.machineDirect(ctx)
	managed, err := connectivity.ConnectMachineRealmManaged(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { _ = managed.Close(); _ = managed.Connection().Close() })
	require.Eventually(t, func() bool { return managed.Path() == xconn.PathDirect }, 20*time.Second,
		50*time.Millisecond, "the machine is reachable directly")

	routed, err := connectivity.ConnectMachineRealmQUIC(ctx, cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { _ = routed.Close(); _ = routed.Connection().Close() })
	require.Never(t, func() bool { return routed.Path() != xconn.PathRouted }, 3*time.Second, 50*time.Millisecond)
}
