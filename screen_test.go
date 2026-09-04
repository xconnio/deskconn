package deskconn_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Lock has real side effects on non-linux platforms")
	}

	ls := &deskconn.Screen{}

	err := ls.Lock()
	require.EqualError(t, err, "screen lock provider not initialized")
}

func TestIsLocked(t *testing.T) {
	ls := &deskconn.Screen{}

	_, err := ls.IsLocked()
	switch runtime.GOOS {
	case goosWindows:
		require.EqualError(t, err, "lock state query is not supported on windows")
	case "darwin":
		require.EqualError(t, err, "lock state query is not supported on macos")
	default:
		require.EqualError(t, err, "screen lock provider not initialized")
	}
}

func mockBacklightDir(t *testing.T) string {
	t.Helper()

	tmp := t.TempDir()

	dev := filepath.Join(tmp, "intel_backlight")
	err := os.Mkdir(dev, 0755)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dev, "max_brightness"), []byte("100"), 0600)
	require.NoError(t, err)

	err = os.WriteFile(filepath.Join(dev, "brightness"), []byte("20"), 0600)
	require.NoError(t, err)

	old := deskconn.BacklightBasePath
	t.Cleanup(func() { deskconn.BacklightBasePath = old })
	deskconn.BacklightBasePath = tmp

	return tmp
}

func TestNewBrightnessDeviceFound(t *testing.T) {
	mockBacklightDir(t)

	// D-Bus isn't available on every platform/CI environment; NewScreen handles a nil conn
	// gracefully (these tests exercise the file-backed brightness path, not D-Bus), so a
	// connect failure here isn't a test failure.
	conn, _ := dbus.ConnectSystemBus()
	sessionConn, _ := dbus.ConnectSessionBus()
	b := deskconn.NewScreen(sessionConn, conn, t.TempDir())

	brightness, err := b.GetBrightness()
	require.NoError(t, err)
	require.Equal(t, 20, brightness)
}

func TestNewBrightnessNoDevice(t *testing.T) {
	old := deskconn.BacklightBasePath
	defer func() { deskconn.BacklightBasePath = old }()

	deskconn.BacklightBasePath = t.TempDir()

	// D-Bus isn't available on every platform/CI environment; NewScreen handles a nil conn
	// gracefully (these tests exercise the file-backed brightness path, not D-Bus), so a
	// connect failure here isn't a test failure.
	conn, _ := dbus.ConnectSystemBus()
	sessionConn, _ := dbus.ConnectSessionBus()
	b := deskconn.NewScreen(sessionConn, conn, t.TempDir())

	err := b.SetBrightness(70)
	require.EqualError(t, err, "brightness device not available")

	_, err = b.GetBrightness()
	require.EqualError(t, err, "brightness device not available")
}

func TestGetBrightness(t *testing.T) {
	mockBacklightDir(t)

	// D-Bus isn't available on every platform/CI environment; NewScreen handles a nil conn
	// gracefully (these tests exercise the file-backed brightness path, not D-Bus), so a
	// connect failure here isn't a test failure.
	conn, _ := dbus.ConnectSystemBus()
	sessionConn, _ := dbus.ConnectSessionBus()
	b := deskconn.NewScreen(sessionConn, conn, t.TempDir())
	value, err := b.GetBrightness()
	require.NoError(t, err)
	require.Equal(t, 20, value)
}

func TestGetBrightnessFileError(t *testing.T) {
	tmp := mockBacklightDir(t)

	// Remove brightness file to trigger error
	err := os.Remove(filepath.Join(tmp, "intel_backlight", "brightness"))
	require.NoError(t, err)

	deskconn.BacklightBasePath = tmp

	// D-Bus isn't available on every platform/CI environment; NewScreen handles a nil conn
	// gracefully (these tests exercise the file-backed brightness path, not D-Bus), so a
	// connect failure here isn't a test failure.
	conn, _ := dbus.ConnectSystemBus()
	sessionConn, _ := dbus.ConnectSessionBus()
	b := deskconn.NewScreen(sessionConn, conn, t.TempDir())

	_, err = b.GetBrightness()
	require.Error(t, err)
}
