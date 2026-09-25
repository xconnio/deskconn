package common

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PrintMode is a purely local machine setting (read/written straight to
// config.yml, no device/network round trip) so both the CLI (`deskconn
// print enable/disable/status`) and the device-side print handler can read
// and change it directly.
type PrintMode string

type PrinterInfo struct {
	Name     string `json:"name"`
	PPDModel string `json:"ppd"`
}

func CurrentPrintMode() (PrintMode, error) {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		return PrintModeDisabled, err
	}
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil {
		if os.IsNotExist(err) {
			return PrintModeDisabled, nil
		}
		return PrintModeDisabled, err
	}
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return PrintModeDisabled, err
	}
	switch config.Printing.Mode {
	case PrintModeAccept, PrintModeHost:
		return config.Printing.Mode, nil
	case PrintModeDisabled, "":
		return PrintModeDisabled, nil
	default:
		return PrintModeDisabled, fmt.Errorf("unsupported print mode %q", config.Printing.Mode)
	}
}
