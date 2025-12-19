package deskconn

import (
	"errors"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	mprisPath   = "/org/mpris/MediaPlayer2"
	playerIface = "org.mpris.MediaPlayer2.Player"
)

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

func (m *MPRIS) allPlayers() ([]dbus.BusObject, error) {
	players, err := m.mprisPlayers()
	if err != nil {
		return nil, err
	}

	var busObjects []dbus.BusObject
	for _, bus := range players {
		busObjects = append(busObjects, m.conn.Object(bus, mprisPath))
	}

	if len(busObjects) == 0 {
		return nil, errors.New("no active players")
	}

	return busObjects, nil
}

func call(objs []dbus.BusObject, method string) error {
	var lastErr error
	for _, obj := range objs {
		if err := obj.Call(playerIface+"."+method, 0).Err; err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (m *MPRIS) PlayPause() error {
	objs, err := m.allPlayers()
	if err != nil {
		return err
	}
	return call(objs, "PlayPause")
}

func (m *MPRIS) Play() error {
	objs, err := m.allPlayers()
	if err != nil {
		return err
	}
	return call(objs, "Play")
}

func (m *MPRIS) Pause() error {
	objs, err := m.allPlayers()
	if err != nil {
		return err
	}
	return call(objs, "Pause")
}

func (m *MPRIS) PlayPausePlayer(name string) error {
	obj := m.conn.Object(name, mprisPath)
	return obj.Call(playerIface+".PlayPause", 0).Err
}

func (m *MPRIS) PlayPlayer(name string) error {
	obj := m.conn.Object(name, mprisPath)
	return obj.Call(playerIface+".Play", 0).Err
}

func (m *MPRIS) PausePlayer(name string) error {
	obj := m.conn.Object(name, mprisPath)
	return obj.Call(playerIface+".Pause", 0).Err
}
