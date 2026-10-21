package userd_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// Only a machine's owners and admins may enable or disable apps. Remote callers reach
// capabilities.set through xconn's bridge, which keeps their roles.
func TestCapabilitiesSetNeedsManager(t *testing.T) {
	const machineRealm = "machine"
	dir := t.TempDir()
	appRouter, appListener, appSession, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(appRouter.Close)
	t.Cleanup(func() { _ = appListener.Close() })

	caps, err := userd.NewCapabilities([]*userd.App{{ID: "screenshot", Name: "Screenshot"}}, dir, appSession)
	require.NoError(t, err)
	require.NoError(t, appSession.Register(common.ProcedureCapabilitiesSet, caps.Set()).Do().Err)

	device, err := xconnd.NewDeviceRouter(machineRealm)
	require.NoError(t, err)
	t.Cleanup(device.Close)
	bridge, err := xconn.ConnectInMemory(device, machineRealm)
	require.NoError(t, err)
	require.NoError(t, xconnd.RegisterBridge(bridge, &xconnd.App{
		Session: appSession,
		NewSession: func(authID, authRole string) (*xconn.Session, error) {
			return connectAs(appRouter, common.LocalRealm, authID, authRole)
		},
		Prefixes: []string{common.ProcedurePrefix, common.ProcedureAdminPrefix},
	}))

	set := func(t *testing.T, caller *xconn.Session, enabled bool) error {
		t.Helper()
		return caller.Call(common.ProcedureCapabilitiesSet).Args("screenshot", enabled).Do().Err
	}

	for _, role := range []string{common.RoleOwner, common.RoleAdmin} {
		t.Run(role, func(t *testing.T) {
			caller, err := connectAs(device, machineRealm, "alice", role)
			require.NoError(t, err)
			require.NoError(t, set(t, caller, false))
			require.False(t, caps.Enabled("screenshot"))
			require.NoError(t, set(t, caller, true))
			require.True(t, caps.Enabled("screenshot"))
		})
	}

	t.Run("member", func(t *testing.T) {
		caller, err := connectAs(device, machineRealm, "bob", "member")
		require.NoError(t, err)
		require.ErrorContains(t, set(t, caller, false), common.ErrNotAuthorized)
		require.True(t, caps.Enabled("screenshot"))
	})

	// Cloud callers get deskconn-router's roles, which the machine router doesn't know.
	t.Run("cloud", func(t *testing.T) {
		for role, allowed := range map[string]bool{
			common.CloudRoleOwner: true, common.CloudRoleAdmin: true, "user:member": false,
		} {
			caller, err := connectAs(appRouter, common.LocalRealm, "carol", role)
			require.NoError(t, err)
			err = set(t, caller, true)
			if allowed {
				require.NoError(t, err, role)
			} else {
				require.ErrorContains(t, err, common.ErrNotAuthorized, role)
			}
		}
	})

	// Sessions on the local socket aren't managers either.
	t.Run("local", func(t *testing.T) {
		caller, err := xconn.ConnectInMemory(appRouter, common.LocalRealm)
		require.NoError(t, err)
		require.ErrorContains(t, set(t, caller, false), common.ErrNotAuthorized)
	})
}

// connectAs joins realm on router in memory, as authID with authRole.
func connectAs(router *xconn.Router, realm, authID, authRole string) (*xconn.Session, error) {
	base, err := xconn.ConnectInMemoryBase(router, realm, authID, authRole, &serializers.MsgPackSerializer{}, 16)
	if err != nil {
		return nil, err
	}
	return xconn.NewSession(base, base.Serializer()), nil
}
