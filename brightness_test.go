package deskconnd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconnd"
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

	old := deskconnd.BacklightBasePath
	t.Cleanup(func() { deskconnd.BacklightBasePath = old })
	deskconnd.BacklightBasePath = tmp

	return tmp
}

func TestNewBrightnessDeviceFound(t *testing.T) {
	mockBacklightDir(t)

	b := deskconnd.NewBrightness()

	err := b.SetBrightness(70)
	require.NoError(t, err)

	brightness, err := b.GetBrightness()
	require.NoError(t, err)
	require.Equal(t, 70, brightness)
}

func TestNewBrightnessNoDevice(t *testing.T) {
	old := deskconnd.BacklightBasePath
	defer func() { deskconnd.BacklightBasePath = old }()

	deskconnd.BacklightBasePath = t.TempDir()

	b := deskconnd.NewBrightness()

	err := b.SetBrightness(70)
	require.EqualError(t, err, "brightness device not available")

	_, err = b.GetBrightness()
	require.EqualError(t, err, "brightness device not available")
}

func TestGetBrightness(t *testing.T) {
	mockBacklightDir(t)

	b := deskconnd.NewBrightness()
	value, err := b.GetBrightness()
	require.NoError(t, err)
	require.Equal(t, 20, value)
}

func TestSetBrightness(t *testing.T) {
	mockBacklightDir(t)

	b := deskconnd.NewBrightness()

	tests := []struct {
		input    int
		expected string
	}{
		{50, "50"},
		{0, "1"},
		{106, "100"},
		{-5, "1"},
	}

	for _, tt := range tests {
		err := b.SetBrightness(tt.input)
		require.NoError(t, err)

		raw, err := os.ReadFile(deskconnd.BacklightBasePath + "/intel_backlight/brightness")
		require.NoError(t, err)
		require.Equal(t, tt.expected, string(raw))
	}
}

func TestGetBrightnessFileError(t *testing.T) {
	tmp := mockBacklightDir(t)

	// Remove brightness file to trigger error
	err := os.Remove(filepath.Join(tmp, "intel_backlight", "brightness"))
	require.NoError(t, err)

	deskconnd.BacklightBasePath = tmp

	b := deskconnd.NewBrightness()

	_, err = b.GetBrightness()
	require.Error(t, err)
}
