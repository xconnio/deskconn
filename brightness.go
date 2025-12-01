package deskconn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var BacklightBasePath = "/sys/class/backlight" //nolint: gochecknoglobals

type Brightness struct {
	brightnessFilePath string
	maxBrightness      int
	deviceExists       bool
}

func NewBrightness() *Brightness {
	entries, err := os.ReadDir(BacklightBasePath)
	if err != nil {
		return &Brightness{deviceExists: false}
	}

	var device string
	for _, e := range entries {
		full := filepath.Join(BacklightBasePath, e.Name())

		info, err := os.Stat(full)
		if err != nil {
			continue
		}

		if info.IsDir() {
			device = full
			break
		}
	}

	if device == "" {
		return &Brightness{deviceExists: false}
	}

	b := &Brightness{
		brightnessFilePath: filepath.Join(device, "brightness"),
		deviceExists:       true,
	}

	raw, err := os.ReadFile(filepath.Join(device, "max_brightness"))
	if err != nil {
		return &Brightness{deviceExists: false}
	}
	b.maxBrightness, _ = strconv.Atoi(strings.TrimSpace(string(raw)))

	return b
}

func (b *Brightness) GetBrightness() (int, error) {
	if !b.deviceExists {
		return 0, fmt.Errorf("brightness device not available")
	}

	raw, err := os.ReadFile(b.brightnessFilePath)
	if err != nil {
		return 0, err
	}

	current, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	percent := (current * 100) / b.maxBrightness

	return percent, nil
}

func (b *Brightness) SetBrightness(percent int) error {
	if !b.deviceExists {
		return fmt.Errorf("brightness device not available")
	}

	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}

	value := (percent * b.maxBrightness) / 100

	if err := os.WriteFile(b.brightnessFilePath, []byte(strconv.Itoa(value)), 0600); err != nil {
		return fmt.Errorf("failed to write brightness value (%d): %w", value, err)
	}

	return nil
}
