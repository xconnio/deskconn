package deskconnd_test

import (
	"encoding/json"
	"os"
	"os/exec"
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

	d := deskconnd.NewDeskconn(nil, false, t.TempDir())
	require.NoError(t, d.Register(callee))

	enc, err := common.ClientKeyExchange(caller)
	require.NoError(t, err)
	rawData, err := common.EncryptedCall(caller, common.ProcedureMachineInfo, nil, enc)
	require.NoError(t, err)

	var machineInfo common.MachineInfo
	require.NoError(t, json.Unmarshal(rawData, &machineInfo))
	require.NotNil(t, machineInfo.Battery)
	require.Equal(t, "Full", machineInfo.Battery.Status)
	require.Equal(t, 100, machineInfo.Battery.Percentage)
}

func TestMachineIsDesktop(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, true, t.TempDir())
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureMachineIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.True(t, isDesktop)
}

func TestMachineIsDesktopFalseOnServer(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, false, t.TempDir())
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureMachineIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.False(t, isDesktop)
}

func TestEncryptedProceduresRequireKeyExchange(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, false, t.TempDir())
	require.NoError(t, d.Register(callee))

	for _, procedure := range []string{
		common.ProcedureMachineInfo,
		common.ProcedureProcessList,
		common.ProcedureProcessSignal,
		common.ProcedureAppList,
		common.ProcedureAppIcon,
		common.ProcedureShellIsBusy,
	} {
		callResp := caller.Call(procedure).Do()
		require.ErrorContains(t, callResp.Err, "call key exchange first", procedure)
	}
}

func TestEncryptedProcedures(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, false, t.TempDir())
	require.NoError(t, d.Register(callee))

	enc, err := common.ClientKeyExchange(caller)
	require.NoError(t, err)

	call := func(procedure string, args any) []byte {
		t.Helper()
		payload, err := json.Marshal(args)
		require.NoError(t, err)
		result, err := common.EncryptedCall(caller, procedure, payload, enc)
		require.NoError(t, err, procedure)
		return result
	}

	var procs []map[string]any
	require.NoError(t, json.Unmarshal(call(common.ProcedureProcessList, nil), &procs))
	require.NotEmpty(t, procs)

	var apps []any
	require.NoError(t, json.Unmarshal(call(common.ProcedureAppList, nil), &apps))

	var busy bool
	require.NoError(t, json.Unmarshal(call(common.ProcedureShellIsBusy, "no-such-shell"), &busy))
	require.False(t, busy)

	sleeper := exec.Command("sleep", "30")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })

	var results []map[string]any
	require.NoError(t, json.Unmarshal(call(common.ProcedureProcessSignal,
		map[string]any{"pids": []int{sleeper.Process.Pid}, "signal": "term"}), &results))
	require.Len(t, results, 1)
	require.Equal(t, true, results[0]["ok"])
}
