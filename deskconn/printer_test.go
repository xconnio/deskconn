package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
)

func saveConfigFile(t *testing.T) func() {
	t.Helper()
	cfgDir, err := common.CfgDirectory()
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

	cfgDir, err := common.CfgDirectory()
	require.NoError(t, err)
	_ = os.Remove(filepath.Join(cfgDir, "config.yml"))

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeDisabled, mode)
}

func TestCurrentPrintModeAccept(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(common.PrintModeAccept))

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeAccept, mode)
}

func TestCurrentPrintModeHost(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(common.PrintModeHost))

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeHost, mode)
}

func TestCurrentPrintModeDisabled(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.SetPrintMode(common.PrintModeAccept))
	require.NoError(t, deskconn.SetPrintMode(common.PrintModeDisabled))

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeDisabled, mode)
}

func TestCurrentPrintModeEmptyModeField(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := common.CfgDirectory()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "config.yml"),
		[]byte("devices: []\n"),
		0600,
	))

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeDisabled, mode)
}

func TestCurrentPrintModeUnknownMode(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := common.CfgDirectory()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(cfgDir, "config.yml"),
		[]byte("printing:\n  mode: bogus\n"),
		0600,
	))

	_, err = common.CurrentPrintMode()
	require.ErrorContains(t, err, "unsupported print mode")
}

func TestSetPrintModeUnsupported(t *testing.T) {
	err := deskconn.SetPrintMode("invalid-mode")
	require.ErrorContains(t, err, "unsupported print mode")
}

func TestSetPrintModePreservesExistingConfig(t *testing.T) {
	t.Cleanup(saveConfigFile(t))

	cfgDir, err := common.CfgDirectory()
	require.NoError(t, err)

	type minConfig struct {
		Devices []map[string]string `yaml:"devices"`
	}
	seed := minConfig{Devices: []map[string]string{{"authid": "dev1", "id": "id1"}}}
	data, err := yaml.Marshal(seed)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yml"), data, 0600))

	require.NoError(t, deskconn.SetPrintMode(common.PrintModeAccept))

	devices, err := common.DevicesFromCfg(cfgDir)
	require.NoError(t, err)
	require.Len(t, devices, 1, "SetPrintMode must not erase other config sections")
}

func TestEnablePrinting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinting())

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeAccept, mode)
}

func TestEnablePrinterHosting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinterHosting())

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeHost, mode)
}

func TestDisablePrinting(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.EnablePrinting())

	require.NoError(t, deskconn.DisablePrinting())

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeDisabled, mode)
}

func TestDisablePrintingIdempotent(t *testing.T) {
	t.Cleanup(saveConfigFile(t))
	require.NoError(t, deskconn.DisablePrinting())
	require.NoError(t, deskconn.DisablePrinting())

	mode, err := common.CurrentPrintMode()
	require.NoError(t, err)
	require.Equal(t, common.PrintModeDisabled, mode)
}
