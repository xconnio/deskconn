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

const (
	getActive = "GetActive"
	lock      = "Lock"
)

var BacklightBasePath = "/sys/class/backlight" //nolint: gochecknoglobals

type lockProvider struct {
	service string
	path    dbus.ObjectPath
	iface   string
	lock    string
	active  string
}

func lockProviders() []*lockProvider {
	return []*lockProvider{
		{"org.gnome.ScreenSaver", "/org/gnome/ScreenSaver", "org.gnome.ScreenSaver", lock, getActive},
		{"org.freedesktop.ScreenSaver", "/ScreenSaver", "org.freedesktop.ScreenSaver", lock, getActive},
		{"com.canonical.Unity.Session", "/com/canonical/Unity/Session", "com.canonical.Unity.Session", lock, "IsLocked"},
		{"org.cinnamon.ScreenSaver", "/org/cinnamon/ScreenSaver", "org.cinnamon.ScreenSaver", lock, getActive},
		{"org.mate.ScreenSaver", "/org/mate/ScreenSaver", "org.mate.ScreenSaver", lock, getActive},
		{"org.xscreensaver", "/org/xscreensaver/ScreenSaver", "org.xscreensaver.ScreenSaver", lock, getActive},
		{"org.lxqt.ScreenSaver", "/org/lxqt/ScreenSaver", "org.lxqt.ScreenSaver", lock, getActive},
		{"org.xfce.SessionManager", "/org/xfce/SessionManager", "org.xfce.SessionManager", lock, ""},
	}
}

type Screen struct {
	sessionBus   *dbus.Conn
	systemBus    *dbus.Conn
	cfgDirectory string

	lockProvider    *lockProvider
	lockInitialized bool

	brightnessFilePath     string
	maxBrightness          int
	brightnessDeviceName   string
	brightnessDeviceExists bool
}

func (s *Screen) SessionBus() *dbus.Conn { return s.sessionBus }

func NewScreen(sessionBus, systemBus *dbus.Conn, cfgDirectory string) *Screen {
	s := &Screen{
		sessionBus:   sessionBus,
		systemBus:    systemBus,
		cfgDirectory: cfgDirectory,
	}

	if sessionBus != nil {
		for _, p := range lockProviders() {
			obj := sessionBus.Object(p.service, p.path)
			call := obj.Call("org.freedesktop.DBus.Introspectable.Introspect", 0)
			if call.Err != nil &&
				strings.Contains(call.Err.Error(), "org.freedesktop.DBus.Error.ServiceUnknown") {
				continue
			}

			s.lockProvider = p
			s.lockInitialized = call.Err == nil
			break
		}
	}

	entries, err := os.ReadDir(BacklightBasePath)
	if err != nil {
		return s
	}

	for _, e := range entries {
		full := filepath.Join(BacklightBasePath, e.Name())
		info, err := os.Stat(full)
		if err != nil || !info.IsDir() {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(full, "max_brightness"))
		if err != nil {
			continue
		}

		max, _ := strconv.Atoi(strings.TrimSpace(string(raw)))

		s.brightnessDeviceName = e.Name()
		s.brightnessFilePath = filepath.Join(full, "brightness")
		s.maxBrightness = max
		s.brightnessDeviceExists = true
		break
	}

	return s
}

func (s *Screen) GetBrightness() (int, error) {
	if !s.brightnessDeviceExists {
		return 0, fmt.Errorf("brightness device not available")
	}

	raw, err := os.ReadFile(s.brightnessFilePath)
	if err != nil {
		return 0, err
	}

	current, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return (current * 100) / s.maxBrightness, nil
}

func (s *Screen) SetBrightness(percent int) error {
	if !s.brightnessDeviceExists {
		return fmt.Errorf("brightness device not available")
	}
	if s.systemBus == nil {
		return fmt.Errorf("system bus not available")
	}

	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}

	value := (percent * s.maxBrightness) / 100
	if value < 0 || value > math.MaxUint32 {
		return fmt.Errorf("brightness value out of uint32 range: %d", value)
	}

	obj := s.systemBus.Object("org.freedesktop.login1", "/org/freedesktop/login1/session/auto")

	return obj.Call("org.freedesktop.login1.Session.SetBrightness", 0, "backlight", s.brightnessDeviceName,
		uint32(value)).Err
}
