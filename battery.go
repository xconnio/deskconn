package deskconn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var PowerSupplyBasePath = "/sys/class/power_supply" //nolint: gochecknoglobals

type BatteryInfo struct {
	Present      bool    `json:"present"`
	Percentage   int     `json:"percentage"`
	Status       string  `json:"status"`
	Technology   string  `json:"technology,omitempty"`
	Manufacturer string  `json:"manufacturer,omitempty"`
	Model        string  `json:"model,omitempty"`
	CycleCount   int     `json:"cycle_count,omitempty"`
	VoltageNow   float64 `json:"voltage_now"`
	PowerNow     float64 `json:"power_now"`

	EnergyNow        float64 `json:"energy_now"`
	EnergyFull       float64 `json:"energy_full"`
	EnergyFullDesign float64 `json:"energy_full_design"`
	HealthPercent    float64 `json:"health_percent,omitempty"`

	TimeRemainingMins int `json:"time_remaining_mins,omitempty"`
}

func findBatteryDevicePath() (string, bool) {
	entries, err := os.ReadDir(PowerSupplyBasePath)
	if err != nil {
		return "", false
	}

	for _, e := range entries {
		full := filepath.Join(PowerSupplyBasePath, e.Name())
		typ, ok := readSysfsString(filepath.Join(full, "type"))
		if !ok || typ != "Battery" {
			continue
		}

		return full, true
	}

	return "", false
}

func readSysfsString(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

func readSysfsInt(path string) (int64, bool) {
	raw, ok := readSysfsString(path)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func GetBatteryInfo() (*BatteryInfo, error) {
	devicePath, ok := findBatteryDevicePath()
	if !ok {
		return nil, fmt.Errorf("battery not available")
	}

	info := &BatteryInfo{Present: true, Status: "Unknown"}

	if present, ok := readSysfsInt(filepath.Join(devicePath, "present")); ok {
		info.Present = present != 0
	}
	if status, ok := readSysfsString(filepath.Join(devicePath, "status")); ok {
		info.Status = status
	}
	if capacity, ok := readSysfsInt(filepath.Join(devicePath, "capacity")); ok {
		info.Percentage = int(capacity)
	}

	info.Technology, _ = readSysfsString(filepath.Join(devicePath, "technology"))
	info.Manufacturer, _ = readSysfsString(filepath.Join(devicePath, "manufacturer"))
	info.Model, _ = readSysfsString(filepath.Join(devicePath, "model_name"))

	if cycles, ok := readSysfsInt(filepath.Join(devicePath, "cycle_count")); ok {
		info.CycleCount = int(cycles)
	}

	var voltageNow int64
	hasVoltage := false
	if v, ok := readSysfsInt(filepath.Join(devicePath, "voltage_now")); ok {
		voltageNow, hasVoltage = v, true
		info.VoltageNow = float64(v) / 1e6
	}

	energyNow, energyNowOK := readSysfsInt(filepath.Join(devicePath, "energy_now"))
	energyFull, energyFullOK := readSysfsInt(filepath.Join(devicePath, "energy_full"))
	energyFullDesign, energyFullDesignOK := readSysfsInt(filepath.Join(devicePath, "energy_full_design"))
	powerNow, powerNowOK := readSysfsInt(filepath.Join(devicePath, "power_now"))

	if hasVoltage {
		voltage := float64(voltageNow) / 1e6

		if !energyNowOK {
			if chargeNow, ok := readSysfsInt(filepath.Join(devicePath, "charge_now")); ok {
				energyNow, energyNowOK = int64(float64(chargeNow)*voltage), true
			}
		}
		if !energyFullOK {
			if chargeFull, ok := readSysfsInt(filepath.Join(devicePath, "charge_full")); ok {
				energyFull, energyFullOK = int64(float64(chargeFull)*voltage), true
			}
		}
		if !energyFullDesignOK {
			if chargeFullDesign, ok := readSysfsInt(filepath.Join(devicePath, "charge_full_design")); ok {
				energyFullDesign, energyFullDesignOK = int64(float64(chargeFullDesign)*voltage), true
			}
		}
		if !powerNowOK {
			if currentNow, ok := readSysfsInt(filepath.Join(devicePath, "current_now")); ok {
				powerNow, powerNowOK = int64(float64(currentNow)*voltage), true
			}
		}
	}

	if energyNowOK {
		info.EnergyNow = float64(energyNow) / 1e6
	}
	if energyFullOK {
		info.EnergyFull = float64(energyFull) / 1e6
	}
	if energyFullDesignOK {
		info.EnergyFullDesign = float64(energyFullDesign) / 1e6
	}
	if powerNowOK {
		info.PowerNow = float64(powerNow) / 1e6
	}

	if info.Percentage == 0 && energyNowOK && energyFullOK && energyFull > 0 {
		info.Percentage = int(float64(energyNow) / float64(energyFull) * 100)
	}

	if energyFullOK && energyFullDesignOK && energyFullDesign > 0 {
		info.HealthPercent = float64(energyFull) / float64(energyFullDesign) * 100
	}

	if powerNowOK && powerNow > 0 {
		switch info.Status {
		case "Discharging":
			if energyNowOK {
				info.TimeRemainingMins = int(float64(energyNow) / float64(powerNow) * 60)
			}
		case "Charging":
			if energyNowOK && energyFullOK && energyFull > energyNow {
				info.TimeRemainingMins = int(float64(energyFull-energyNow) / float64(powerNow) * 60)
			}
		}
	}

	return info, nil
}
