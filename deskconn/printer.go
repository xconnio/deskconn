package deskconn

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
)

type PrintJobStatus struct {
	JobID     string `json:"job_id"`
	Printer   string `json:"printer"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func EnablePrinting() error {
	return SetPrintMode(common.PrintModeAccept)
}

func EnablePrinterHosting() error {
	return SetPrintMode(common.PrintModeHost)
}

func SetPrintMode(mode common.PrintMode) error {
	switch mode {
	case common.PrintModeAccept, common.PrintModeHost, common.PrintModeDisabled:
		return updatePrintingConfig(mode)
	default:
		return fmt.Errorf("unsupported print mode %q", mode)
	}
}

func DisablePrinting() error {
	return SetPrintMode(common.PrintModeDisabled)
}

func updatePrintingConfig(mode common.PrintMode) error {
	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(cfgDirectory, "config.yml")

	var config common.Config
	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &config); err != nil {
			return err
		}
	}

	config.Printing.Mode = mode
	b, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, b, 0600)
}
