package userd_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

// Each user lists only the settings they saved, and saving a key again replaces it.
func TestSettingsPerUser(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", &xconn.RealmConfig{AutoDiscloseCaller: true,
		Roles: []xconn.RealmRole{{Name: common.RoleMember, Permissions: []xconn.Permission{
			{URI: "io.xconn.", MatchPolicy: wampproto.MatchPrefix, AllowCall: true},
		}}}}))
	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	d := userd.NewDeskconn(false, t.TempDir())
	require.NoError(t, d.Register(callee))
	t.Cleanup(func() { _ = d.CloseSettings() })

	alice, err := connectAs(r, "realm1", "alice", "member")
	require.NoError(t, err)
	bob, err := connectAs(r, "realm1", "bob", "member")
	require.NoError(t, err)

	list := func(caller *xconn.Session) map[string]string {
		resp := caller.Call(common.ProcedureSettingsList).Do()
		require.NoError(t, resp.Err)
		dict, err := resp.ArgDict(0)
		require.NoError(t, err)
		values := map[string]string{}
		for key := range dict {
			values[key], err = dict.String(key)
			require.NoError(t, err)
		}
		return values
	}

	require.Empty(t, list(alice))
	require.NoError(t, alice.Call(common.ProcedureSettingsSave).Args("dock", "left").Do().Err)
	require.NoError(t, alice.Call(common.ProcedureSettingsSave).Args("dock", "right").Do().Err)
	require.NoError(t, alice.Call(common.ProcedureSettingsSave).Args("windows", "[]").Do().Err)
	require.Equal(t, map[string]string{"dock": "right", "windows": "[]"}, list(alice))
	require.Empty(t, list(bob))

	require.Error(t, alice.Call(common.ProcedureSettingsSave).Args("dock").Do().Err)
}
