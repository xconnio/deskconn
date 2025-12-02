package deskconn

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

var BacklightBasePath = "/sys/class/backlight" //nolint: gochecknoglobals

type Brightness struct {
	dbusConn           *dbus.Conn
	brightnessFilePath string
	maxBrightness      int
	deviceName         string
	deviceExists       bool
}

func NewBrightness(conn *dbus.Conn) *Brightness {
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
			device = e.Name()
			break
		}
	}

	if device == "" {
		return &Brightness{deviceExists: false}
	}

	b := &Brightness{
		deviceName:         device,
		brightnessFilePath: filepath.Join(BacklightBasePath, device, "brightness"),
		deviceExists:       true,
	}

	raw, err := os.ReadFile(filepath.Join(BacklightBasePath, device, "max_brightness"))
	if err != nil {
		return &Brightness{deviceExists: false}
	}
	b.maxBrightness, _ = strconv.Atoi(strings.TrimSpace(string(raw)))

	b.dbusConn = conn

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
	if value < 0 || value > math.MaxUint32 {
		return fmt.Errorf("brightness value out of uint32 range: %d", value)
	}

	obj := b.dbusConn.Object("org.freedesktop.login1", "/org/freedesktop/login1/session/auto")
	return obj.Call("org.freedesktop.login1.Session.SetBrightness", 0, "backlight", b.deviceName, uint32(value)).Err
}
