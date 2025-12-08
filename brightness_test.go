package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

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

	conn, err := dbus.ConnectSystemBus()
	require.NoError(t, err)
	b := deskconn.NewBrightness(conn)

	brightness, err := b.GetBrightness()
	require.NoError(t, err)
	require.Equal(t, 20, brightness)
}

func TestNewBrightnessNoDevice(t *testing.T) {
	old := deskconn.BacklightBasePath
	defer func() { deskconn.BacklightBasePath = old }()

	deskconn.BacklightBasePath = t.TempDir()

	conn, err := dbus.ConnectSystemBus()
	require.NoError(t, err)
	b := deskconn.NewBrightness(conn)

	err = b.SetBrightness(70)
	require.EqualError(t, err, "brightness device not available")

	_, err = b.GetBrightness()
	require.EqualError(t, err, "brightness device not available")
}

func TestGetBrightness(t *testing.T) {
	mockBacklightDir(t)

	conn, err := dbus.ConnectSystemBus()
	require.NoError(t, err)
	b := deskconn.NewBrightness(conn)
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

	conn, err := dbus.ConnectSystemBus()
	require.NoError(t, err)
	b := deskconn.NewBrightness(conn)

	_, err = b.GetBrightness()
	require.Error(t, err)
}
