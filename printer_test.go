package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn"
)

func saveConfigFile(t *testing.T) func() {
	t.Helper()
	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	cfgPath := filepath.Join(cfgDir, "config.yml")

	original, readErr := os.ReadFile(cfgPath)
	return func() {
		if readErr == nil {
			_ = os.WriteFile(cfgPath, original, 0600)
		} else {
			_ = os.Remove(cfgPath)
		}
	}
}

func TestCurrentPrintModeNoConfig(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	_ = os.Remove(filepath.Join(cfgDir, "config.yml"))

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeDisabled, mode)
}

func TestCurrentPrintModeAccept(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(deskconn.PrintModeAccept))

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeAccept, mode)
}

func TestCurrentPrintModeHost(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(deskconn.PrintModeHost))

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeHost, mode)
}

func TestCurrentPrintModeDisabled(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(deskconn.PrintModeAccept))
	require.NoError(t, deskconn.SetPrintMode(deskconn.PrintModeDisabled))

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeDisabled, mode)
}

func TestCurrentPrintModeEmptyModeField(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "config.yml"),
		[]byte("devices: []\n"),
		0600,
	))

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeDisabled, mode)
}

func TestCurrentPrintModeUnknownMode(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "config.yml"),
		[]byte("printing:\n  mode: bogus\n"),
		0600,
	))

	_, err = deskconn.CurrentPrintMode()
	require.ErrorContains(t, err, "unsupported print mode")
}

func TestSetPrintModeUnsupported(t *testing.T) {
	err := deskconn.SetPrintMode("invalid-mode")
	require.ErrorContains(t, err, "unsupported print mode")
}

func TestSetPrintModePreservesExistingConfig(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := deskconn.CfgDirectory()
	require.NoError(t, err)

	type minConfig struct {
		Devices []map[string]string `yaml:"devices"`
	}
	seed := minConfig{Devices: []map[string]string{{"authid": "dev1", "id": "id1"}}}
	data, err := yaml.Marshal(seed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yml"), data, 0600))

	require.NoError(t, deskconn.SetPrintMode(deskconn.PrintModeAccept))

	devices, err := deskconn.DevicesFromCfg(cfgDir)
	require.NoError(t, err)
	require.Len(t, devices, 1, "SetPrintMode must not erase other config sections")
}

func TestEnablePrinting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinting())

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeAccept, mode)
}

func TestEnablePrinterHosting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinterHosting())

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeHost, mode)
}

func TestDisablePrinting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinting())

	require.NoError(t, deskconn.DisablePrinting())

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeDisabled, mode)
}

func TestDisablePrintingIdempotent(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.DisablePrinting())
	require.NoError(t, deskconn.DisablePrinting())

	mode, err := deskconn.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, deskconn.PrintModeDisabled, mode)
}
