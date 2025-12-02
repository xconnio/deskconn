package deskconn

import (
	"fmt"
	"math"

	"github.com/godbus/dbus/v5"
)

type Brightness struct {
	conn *dbus.Conn
}

func NewBrightness() *Brightness {
	conn, _ := dbus.ConnectSessionBus()
	return &Brightness{conn: conn}
}

func (b *Brightness) GetBrightness() (int, error) {
	if b.conn != nil {
		if v, err := b.getGNOME(); err == nil {
			return v, nil
		}
		if v, err := b.getKDE(); err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("brightness not available")
}

func (b *Brightness) SetBrightness(percent int) error {
	if percent < 1 {
		percent = 1
	}
	if percent > 100 {
		percent = 100
	}

	if b.conn != nil {
		if err := b.setGNOME(percent); err == nil {
			return nil
		}
		if err := b.setKDE(percent); err == nil {
			return nil
		}
	}
	return fmt.Errorf("failed to set brightness")
}

func (b *Brightness) getGNOME() (int, error) {
	obj := b.conn.Object("org.gnome.SettingsDaemon.Power", "/org/gnome/SettingsDaemon/Power")
	var val int32
	err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.gnome.SettingsDaemon.Power.Screen", "Brightness").Store(&val)
	if err != nil {
		return 0, err
	}
	return int(val), nil
}

func (b *Brightness) setGNOME(percent int) error {
	obj := b.conn.Object("org.gnome.SettingsDaemon.Power", "/org/gnome/SettingsDaemon/Power")

	if percent > math.MaxInt32 || percent < math.MinInt32 {
		return fmt.Errorf("brightness value out of int32 range")
	}

	return obj.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.gnome.SettingsDaemon.Power.Screen", "Brightness",
		dbus.MakeVariant(int32(percent))).Err
}

func (b *Brightness) getKDE() (int, error) {
	obj := b.conn.Object("org.kde.Solid.PowerManagement",
		"/org/kde/Solid/PowerManagement/Actions/BrightnessControl")

	var brightness int32
	err := obj.Call("org.kde.Solid.PowerManagement.Actions.BrightnessControl.brightness",
		0).Store(&brightness)
	if err != nil {
		return 0, err
	}
	return int(brightness), nil
}

func (b *Brightness) setKDE(percent int) error {
	obj := b.conn.Object("org.kde.Solid.PowerManagement",
		"/org/kde/Solid/PowerManagement/Actions/BrightnessControl")

	if percent > math.MaxInt32 || percent < math.MinInt32 {
		return fmt.Errorf("brightness value out of int32 range")
	}

	return obj.Call("org.kde.Solid.PowerManagement.Actions.BrightnessControl.setBrightness",
		0, int32(percent)).Err
}
