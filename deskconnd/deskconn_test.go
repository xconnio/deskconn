package deskconnd_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
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

func TestMachineInfoIncludesBattery(t *testing.T) {
	tmp := t.TempDir()
	dev := filepath.Join(tmp, "BAT0")
	require.NoError(t, os.Mkdir(dev, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "type"), []byte("Battery"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "status"), []byte("Full"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "capacity"), []byte("100"), 0600))

	old := deskconnd.PowerSupplyBasePath
	defer func() { deskconnd.PowerSupplyBasePath = old }()
	deskconnd.PowerSupplyBasePath = tmp

	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, false)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureMachineInfo).Do()
	require.NoError(t, callResp.Err)

	rawData, err := callResp.ArgBytes(0)
	require.NoError(t, err)

	var machineInfo common.MachineInfo
	require.NoError(t, json.Unmarshal(rawData, &machineInfo))
	require.NotNil(t, machineInfo.Battery)
	require.Equal(t, "Full", machineInfo.Battery.Status)
	require.Equal(t, 100, machineInfo.Battery.Percentage)
}

func TestMachineIsDesktop(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, true)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureMachineIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.True(t, isDesktop)
}

func TestMachineIsDesktopFalseOnServer(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, false)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureMachineIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.False(t, isDesktop)
}
