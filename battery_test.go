package deskconn_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

const statusKey = "status"

func mockPowerSupplyDir(t *testing.T, files map[string]string) {
	t.Helper()

	tmp := t.TempDir()
	dev := filepath.Join(tmp, "BAT0")
	require.NoError(t, os.Mkdir(dev, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "type"), []byte("Battery"), 0600))

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dev, name), []byte(content), 0600))
	}

	old := deskconn.PowerSupplyBasePath
	t.Cleanup(func() { deskconn.PowerSupplyBasePath = old })
	deskconn.PowerSupplyBasePath = tmp
}

func TestBatteryNoDevice(t *testing.T) {
	old := deskconn.PowerSupplyBasePath
	defer func() { deskconn.PowerSupplyBasePath = old }()
	deskconn.PowerSupplyBasePath = t.TempDir()

	_, err := deskconn.GetBatteryInfo()
	require.EqualError(t, err, "battery not available")
}

func TestBatteryEnergyBased(t *testing.T) {
	mockPowerSupplyDir(t, map[string]string{
		statusKey:            "Discharging",
		"capacity":           "55",
		"technology":         "Li-ion",
		"manufacturer":       "ACME",
		"model_name":         "BAT9000",
		"cycle_count":        "42",
		"voltage_now":        "12000000",
		"energy_now":         "27500000",
		"energy_full":        "50000000",
		"energy_full_design": "55000000",
		"power_now":          "11000000",
	})

	info, err := deskconn.GetBatteryInfo()
	require.NoError(t, err)

	require.True(t, info.Present)
	require.Equal(t, 55, info.Percentage)
	require.Equal(t, "Discharging", info.Status)
	require.Equal(t, "Li-ion", info.Technology)
	require.Equal(t, "ACME", info.Manufacturer)
	require.Equal(t, "BAT9000", info.Model)
	require.Equal(t, 42, info.CycleCount)
	require.InDelta(t, 12.0, info.VoltageNow, 0.001)
	require.InDelta(t, 11.0, info.PowerNow, 0.001)
	require.InDelta(t, 27.5, info.EnergyNow, 0.001)
	require.InDelta(t, 50.0, info.EnergyFull, 0.001)
	require.InDelta(t, 55.0, info.EnergyFullDesign, 0.001)
	require.InDelta(t, 90.909, info.HealthPercent, 0.01)
	require.Equal(t, 150, info.TimeRemainingMins)
}

func TestBatteryChargeBased(t *testing.T) {
	mockPowerSupplyDir(t, map[string]string{
		statusKey:            "Charging",
		"technology":         "Li-poly",
		"voltage_now":        "10000000",
		"charge_now":         "3000000",
		"charge_full":        "5000000",
		"charge_full_design": "5500000",
		"current_now":        "1000000",
	})

	info, err := deskconn.GetBatteryInfo()
	require.NoError(t, err)

	require.Equal(t, "Charging", info.Status)
	require.Equal(t, 60, info.Percentage)
	require.InDelta(t, 30.0, info.EnergyNow, 0.001)
	require.InDelta(t, 50.0, info.EnergyFull, 0.001)
	require.InDelta(t, 55.0, info.EnergyFullDesign, 0.001)
	require.InDelta(t, 10.0, info.PowerNow, 0.001)
	require.Equal(t, 120, info.TimeRemainingMins)
}

func TestDeviceInfoIncludesBattery(t *testing.T) {
	mockPowerSupplyDir(t, map[string]string{
		statusKey:  "Full",
		"capacity": "100",
	})

	callee, caller := setupRouterAndConnectSessions(t)

	d := deskconn.NewDeskconn(nil, nil, nil)
	require.NoError(t, d.Register(callee))

	callResp := caller.Call(deskconn.ProcedureDeviceInfo).Do()
	require.NoError(t, callResp.Err)

	rawData, err := callResp.ArgBytes(0)
	require.NoError(t, err)

	var info deskconn.DeviceInfo
	require.NoError(t, json.Unmarshal(rawData, &info))
	require.NotNil(t, info.Battery)
	require.Equal(t, "Full", info.Battery.Status)
	require.Equal(t, 100, info.Battery.Percentage)
}
