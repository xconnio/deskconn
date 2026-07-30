package info_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/info"
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

	old := info.PowerSupplyBasePath
	t.Cleanup(func() { info.PowerSupplyBasePath = old })
	info.PowerSupplyBasePath = tmp
}

func TestBatteryNoDevice(t *testing.T) {
	old := info.PowerSupplyBasePath
	defer func() { info.PowerSupplyBasePath = old }()
	info.PowerSupplyBasePath = t.TempDir()

	_, err := info.GetBatteryInfo()
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

	batteryInfo, err := info.GetBatteryInfo()
	require.NoError(t, err)

	require.True(t, batteryInfo.Present)
	require.Equal(t, 55, batteryInfo.Percentage)
	require.Equal(t, "Discharging", batteryInfo.Status)
	require.Equal(t, "Li-ion", batteryInfo.Technology)
	require.Equal(t, "ACME", batteryInfo.Manufacturer)
	require.Equal(t, "BAT9000", batteryInfo.Model)
	require.Equal(t, 42, batteryInfo.CycleCount)
	require.InDelta(t, 12.0, batteryInfo.VoltageNow, 0.001)
	require.InDelta(t, 11.0, batteryInfo.PowerNow, 0.001)
	require.InDelta(t, 27.5, batteryInfo.EnergyNow, 0.001)
	require.InDelta(t, 50.0, batteryInfo.EnergyFull, 0.001)
	require.InDelta(t, 55.0, batteryInfo.EnergyFullDesign, 0.001)
	require.InDelta(t, 90.909, batteryInfo.HealthPercent, 0.01)
	require.Equal(t, 150, batteryInfo.TimeRemainingMins)
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

	batteryInfo, err := info.GetBatteryInfo()
	require.NoError(t, err)

	require.Equal(t, "Charging", batteryInfo.Status)
	require.Equal(t, 60, batteryInfo.Percentage)
	require.InDelta(t, 30.0, batteryInfo.EnergyNow, 0.001)
	require.InDelta(t, 50.0, batteryInfo.EnergyFull, 0.001)
	require.InDelta(t, 55.0, batteryInfo.EnergyFullDesign, 0.001)
	require.InDelta(t, 10.0, batteryInfo.PowerNow, 0.001)
	require.Equal(t, 120, batteryInfo.TimeRemainingMins)
}
