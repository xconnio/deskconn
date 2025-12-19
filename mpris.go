package deskconn

import (
	"strings"

	"github.com/godbus/dbus/v5"
)

const mprisPath = "/org/mpris/MediaPlayer2"

type MPRIS struct {
	conn *dbus.Conn
}

func NewMPRIS(conn *dbus.Conn) *MPRIS {
	return &MPRIS{conn: conn}
}

func (m *MPRIS) mprisPlayers() ([]string, error) {
	obj := m.conn.Object("org.freedesktop.DBus", "/org/freedesktop/DBus")

	var names []string
	if err := obj.Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return nil, err
	}

	var players []string
	for _, n := range names {
		if strings.HasPrefix(n, "org.mpris.MediaPlayer2.") {
			players = append(players, n)
		}
	}

	return players, nil
}

func (m *MPRIS) playerIdentity(bus string) (string, error) {
	obj := m.conn.Object(bus, mprisPath)

	var v dbus.Variant
	err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.mpris.MediaPlayer2", "Identity").Store(&v)
	if err != nil {
		return "", err
	}

	return v.String(), nil
}

func (m *MPRIS) ListPlayers() (map[string]string, error) {
	players, err := m.mprisPlayers()
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)

	for _, bus := range players {
		name, err := m.playerIdentity(bus)
		if err == nil {
			result[bus] = name
		} else {
			result[bus] = bus
		}
	}

	return result, nil
}
