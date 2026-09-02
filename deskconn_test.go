package deskconn_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/deskconn/info"
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
	screen := deskconn.NewScreen(sessionConn, conn, t.TempDir())
	mpris := deskconn.NewMPRIS(sessionConn)
	audio := deskconn.NewAudio()
	defer audio.Close()
	d := deskconn.NewDeskconn(screen, mpris, audio, true)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(deskconn.ProcedureScreenBrightnessGet).Do()
	if callResp.Err != nil {
		// Headless / DBus unavailable case
		require.ErrorContains(t, callResp.Err, "brightness device not available")
		return
	}

	initial := int(callResp.ArgInt64Or(0, 0))
	require.GreaterOrEqual(t, initial, 0)
	require.LessOrEqual(t, initial, 100)

	callResp = caller.Call(deskconn.ProcedureScreenBrightnessSet).Do()
	require.ErrorContains(t, callResp.Err, "wamp.error.invalid_argument")

	callResp = caller.Call(deskconn.ProcedureScreenBrightnessSet).Arg(70).Do()
	require.NoError(t, callResp.Err)

	callResp = caller.Call(deskconn.ProcedureScreenBrightnessGet).Do()
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

	old := info.PowerSupplyBasePath
	defer func() { info.PowerSupplyBasePath = old }()
	info.PowerSupplyBasePath = tmp

	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconn.NewDeskconn(nil, nil, nil, false)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(deskconn.ProcedureDeviceInfo).Do()
	require.NoError(t, callResp.Err)

	rawData, err := callResp.ArgBytes(0)
	require.NoError(t, err)

	var deviceInfo info.DeviceInfo
	require.NoError(t, json.Unmarshal(rawData, &deviceInfo))
	require.NotNil(t, deviceInfo.Battery)
	require.Equal(t, "Full", deviceInfo.Battery.Status)
	require.Equal(t, 100, deviceInfo.Battery.Percentage)
}

func TestDeviceIsDesktop(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconn.NewDeskconn(nil, nil, nil, true)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(deskconn.ProcedureDeviceIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.True(t, isDesktop)
}

func TestDeviceIsDesktopFalseOnServer(t *testing.T) {
	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconn.NewDeskconn(nil, nil, nil, false)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(deskconn.ProcedureDeviceIsDesktop).Do()
	require.NoError(t, callResp.Err)

	isDesktop, err := callResp.ArgBool(0)
	require.NoError(t, err)
	require.False(t, isDesktop)
}
