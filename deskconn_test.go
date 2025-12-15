package deskconn_test

import (
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func setupRouterAndConnectSessions(t *testing.T) (*xconn.Session, *xconn.Session) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)

	err = r.AddRealm("realm1", xconn.DefaultRealmConfig())
	require.NoError(t, err)

	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	return callee, caller
}

func TestBrightnessGetSet(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	conn, err := dbus.ConnectSystemBus()
	require.NoError(t, err)
	b := deskconn.NewBrightness(conn)
	d := deskconn.NewDeskconn(callee, b, nil)
	require.NoError(t, d.Start())

	callResp := caller.Call(deskconn.ProcedureBrightnessGet).Do()
	if callResp.Err != nil {
		// Headless / DBus unavailable case
		require.ErrorContains(t, callResp.Err, "brightness device not available")
		return
	}

	initial := int(callResp.ArgInt64Or(0, 0))
	require.GreaterOrEqual(t, initial, 0)
	require.LessOrEqual(t, initial, 100)

	callResp = caller.Call(deskconn.ProcedureBrightnessSet).Do()
	require.ErrorContains(t, callResp.Err, "wamp.error.invalid_argument")

	callResp = caller.Call(deskconn.ProcedureBrightnessSet).Arg(70).Do()
	require.NoError(t, callResp.Err)

	callResp = caller.Call(deskconn.ProcedureBrightnessGet).Do()
	require.NoError(t, callResp.Err)

	updated := int(callResp.ArgInt64Or(0, 0))
	require.GreaterOrEqual(t, updated, 0)
	require.LessOrEqual(t, updated, 100)
}
