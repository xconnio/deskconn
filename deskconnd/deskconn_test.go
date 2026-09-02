package deskconnd_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/xconn-go"
)

// goosWindows is runtime.GOOS's value on Windows, shared by tests that skip or branch on it.
const goosWindows = "windows"

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
	if runtime.GOOS == goosWindows {
		t.Skip("brightness is D-Bus-based and not registered on windows")
	}
	callee, caller := setupRouterAndConnectSessions(t)

	// D-Bus isn't available on every platform/CI environment; NewScreen/NewMPRIS handle a
	// nil conn gracefully, so a connect failure here isn't a test failure.
	conn, _ := dbus.ConnectSystemBus()
	sessionConn, _ := dbus.ConnectSessionBus()
	screen := deskconnd.NewScreen(sessionConn, conn, t.TempDir())
	mpris := deskconnd.NewMPRIS(sessionConn)
	audio := deskconnd.NewAudio()
	defer audio.Close()
	d := deskconnd.NewDeskconn(screen, mpris, audio, true, t.TempDir())
	t.Cleanup(d.Close)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureScreenBrightnessGet).Do()
	if callResp.Err != nil {
		// Headless / DBus unavailable case
		require.ErrorContains(t, callResp.Err, "brightness device not available")
		return
	}

	initial := int(callResp.ArgInt64Or(0, 0))
	require.GreaterOrEqual(t, initial, 0)
	require.LessOrEqual(t, initial, 100)

	callResp = caller.Call(common.ProcedureScreenBrightnessSet).Do()
	require.ErrorContains(t, callResp.Err, "wamp.error.invalid_argument")

	callResp = caller.Call(common.ProcedureScreenBrightnessSet).Arg(70).Do()
	require.NoError(t, callResp.Err)

	callResp = caller.Call(common.ProcedureScreenBrightnessGet).Do()
	require.NoError(t, callResp.Err)

	updated := int(callResp.ArgInt64Or(0, 0))
	require.GreaterOrEqual(t, updated, 0)
	require.LessOrEqual(t, updated, 100)
}

func TestDeviceInfoIncludesBattery(t *testing.T) {
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

	d := deskconnd.NewDeskconn(nil, nil, nil, false, t.TempDir())
	t.Cleanup(d.Close)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureDeviceInfo).Do()
	require.NoError(t, callResp.Err)

	rawData, err := callResp.ArgBytes(0)
	require.NoError(t, err)

	var deviceInfo common.DeviceInfo
	require.NoError(t, json.Unmarshal(rawData, &deviceInfo))
	require.NotNil(t, deviceInfo.Battery)
	require.Equal(t, "Full", deviceInfo.Battery.Status)
	require.Equal(t, 100, deviceInfo.Battery.Percentage)
}

func TestDeviceIsDesktop(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, nil, nil, true, t.TempDir())
	t.Cleanup(d.Close)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureDeviceIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.True(t, isDesktop)
}

func TestDeviceIsDesktopFalseOnServer(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconnd.NewDeskconn(nil, nil, nil, false, t.TempDir())
	t.Cleanup(d.Close)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(common.ProcedureDeviceIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.False(t, isDesktop)
}
