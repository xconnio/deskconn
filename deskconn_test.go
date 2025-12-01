package deskconn_test

import (
	"testing"

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

	mockBacklightDir(t)

	b := deskconn.NewBrightness()
	d := deskconn.NewDeskconn(callee, b)
	require.NoError(t, d.Start())

	callResp := caller.Call(deskconn.ProcedureBrightnessGet).Do()
	require.NoError(t, callResp.Err)
	require.Equal(t, 20, int(callResp.ArgInt64Or(0, 0)))

	// call without required argument
	callResp = caller.Call(deskconn.ProcedureBrightnessSet).Do()
	require.ErrorContains(t, callResp.Err, "wamp.error.invalid_argument")

	callResp = caller.Call(deskconn.ProcedureBrightnessSet).Arg(70).Do()
	require.NoError(t, callResp.Err)

	// verify that brightness was updated
	callResp = caller.Call(deskconn.ProcedureBrightnessGet).Do()
	require.NoError(t, callResp.Err)
	require.Equal(t, 70, int(callResp.ArgInt64Or(0, 0)))
}
