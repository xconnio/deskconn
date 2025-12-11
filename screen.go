package deskconn

import (
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

type lockProvider struct {
	service string
	path    dbus.ObjectPath
	iface   string
	lock    string
	active  string
}

func lockProviders() []*lockProvider {
	return []*lockProvider{
		{"org.gnome.ScreenSaver", "/org/gnome/ScreenSaver", "org.gnome.ScreenSaver", "Lock", "GetActive"},
		{"org.freedesktop.ScreenSaver", "/ScreenSaver", "org.freedesktop.ScreenSaver", "Lock", "GetActive"},
		{"com.canonical.Unity.Session", "/com/canonical/Unity/Session", "com.canonical.Unity.Session", "Lock", "IsLocked"},
		{"org.cinnamon.ScreenSaver", "/org/cinnamon/ScreenSaver", "org.cinnamon.ScreenSaver", "Lock", "GetActive"},
		{"org.mate.ScreenSaver", "/org/mate/ScreenSaver", "org.mate.ScreenSaver", "Lock", "GetActive"},
		{"org.xscreensaver", "/org/xscreensaver/ScreenSaver", "org.xscreensaver.ScreenSaver", "Lock", "GetActive"},
		{"org.lxqt.ScreenSaver", "/org/lxqt/ScreenSaver", "org.lxqt.ScreenSaver", "Lock", "GetActive"},
		{"org.xfce.SessionManager", "/org/xfce/SessionManager", "org.xfce.SessionManager", "Lock", ""},
	}
}

type Screen struct {
	conn        *dbus.Conn
	provider    *lockProvider
	initialized bool
}

func NewScreen(conn *dbus.Conn) *Screen {
	for _, p := range lockProviders() {
		obj := conn.Object(p.service, p.path)
		call := obj.Call("org.freedesktop.DBus.Introspectable.Introspect", 0)
		if call.Err != nil &&
			strings.Contains(call.Err.Error(), "org.freedesktop.DBus.Error.ServiceUnknown") {
			continue
		}

		return &Screen{
			conn:        conn,
			provider:    p,
			initialized: call.Err == nil,
		}
	}

	return &Screen{
		conn:        conn,
		initialized: false,
	}
}

func (ls *Screen) Lock() error {
	if !ls.initialized || ls.provider == nil {
		return fmt.Errorf("screen lock provider not initialized")
	}

	obj := ls.conn.Object(ls.provider.service, ls.provider.path)
	return obj.Call(ls.provider.iface+"."+ls.provider.lock, 0).Err
}

func (ls *Screen) IsLocked() (bool, error) {
	if !ls.initialized || ls.provider == nil {
		return false, fmt.Errorf("screen lock provider not initialized")
	}
	if ls.provider.active == "" {
		return false, fmt.Errorf("provider does not support isLocked")
	}

	obj := ls.conn.Object(ls.provider.service, ls.provider.path)

	var active bool
	err := obj.Call(ls.provider.iface+"."+ls.provider.active, 0).Store(&active)
	return active, err
}
