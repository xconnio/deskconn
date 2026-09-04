package deskconn

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	mprisPath   = "/org/mpris/MediaPlayer2"
	playerIface = "org.mpris.MediaPlayer2.Player"

	maxArtBytes     = 10 << 20 // 10MiB
	artFetchTimeout = 5 * time.Second
)

// PlayerInfo describes an MPRIS player and, if available, its currently playing track.
type PlayerInfo struct {
	Identity       string `json:"identity"`
	Title          string `json:"title"`
	Artist         string `json:"artist"`
	Album          string `json:"album"`
	ArtMimeType    string `json:"art_mime_type,omitempty"`
	ArtData        []byte `json:"art_data,omitempty"`
	PlaybackStatus string `json:"playback_status"`
	CanGoNext      bool   `json:"can_go_next"`
	CanGoPrevious  bool   `json:"can_go_previous"`
	CanPlay        bool   `json:"can_play"`
	CanPause       bool   `json:"can_pause"`
}

type MPRIS struct {
	conn *dbus.Conn
}

func NewMPRIS(conn *dbus.Conn) *MPRIS {
	return &MPRIS{conn: conn}
}

func (m *MPRIS) object(dest string, path dbus.ObjectPath) (dbus.BusObject, error) {
	if m.conn == nil {
		return nil, errors.New("mpris unavailable: no session bus connection")
	}
	return m.conn.Object(dest, path), nil
}

func (m *MPRIS) mprisPlayers() ([]string, error) {
	obj, err := m.object("org.freedesktop.DBus", "/org/freedesktop/DBus")
	if err != nil {
		return nil, err
	}

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
	obj, err := m.object(bus, mprisPath)
	if err != nil {
		return "", err
	}

	var v dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.mpris.MediaPlayer2", "Identity").
		Store(&v); err != nil {
		return "", err
	}

	return v.String(), nil
}

func (m *MPRIS) playerProperties(bus string) (map[string]dbus.Variant, error) {
	obj, err := m.object(bus, mprisPath)
	if err != nil {
		return nil, err
	}

	var props map[string]dbus.Variant
	err = obj.Call("org.freedesktop.DBus.Properties.GetAll", 0, playerIface).Store(&props)
	return props, err
}

// boolPropertyOrTrue returns the bool value of a property. Per the MPRIS spec, CanGoNext,
// CanGoPrevious, CanPlay and CanPause should be assumed true when not exposed by a player,
// so missing or unexpected-type properties also fall back to true.
func boolPropertyOrTrue(props map[string]dbus.Variant, name string) bool {
	v, ok := props[name]
	if !ok {
		return true
	}
	b, ok := v.Value().(bool)
	if !ok {
		return true
	}
	return b
}

func parseMetadata(v dbus.Variant) (title, artist, album, artURL string) {
	meta, ok := v.Value().(map[string]dbus.Variant)
	if !ok {
		return
	}

	if t, ok := meta["xesam:title"]; ok {
		title, _ = t.Value().(string)
	}
	if a, ok := meta["xesam:artist"]; ok {
		switch val := a.Value().(type) {
		case []string:
			artist = strings.Join(val, ", ")
		case string:
			artist = val
		}
	}
	if al, ok := meta["xesam:album"]; ok {
		album, _ = al.Value().(string)
	}
	if au, ok := meta["mpris:artUrl"]; ok {
		artURL, _ = au.Value().(string)
	}

	return
}

// fetchArt resolves an mpris:artUrl to raw image bytes, reading local files directly
// and downloading remote (http/https) art so clients never need to deal with URI schemes.
func fetchArt(artURL string) (mimeType string, data []byte, err error) {
	if artURL == "" {
		return "", nil, nil
	}

	u, err := url.Parse(artURL)
	if err != nil {
		return "", nil, err
	}

	switch u.Scheme {
	case "file", "":
		path := fileURIToPath(artURL)
		data, err = os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		return imageMimeTypeByExt(path), data, nil
	case "http", "https":
		client := http.Client{Timeout: artFetchTimeout}
		resp, err := client.Get(artURL)
		if err != nil {
			return "", nil, err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return "", nil, fmt.Errorf("fetch art: unexpected status %s", resp.Status)
		}

		data, err = io.ReadAll(io.LimitReader(resp.Body, maxArtBytes))
		if err != nil {
			return "", nil, err
		}

		mimeType = resp.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = imageMimeTypeByExt(u.Path)
		}
		return mimeType, data, nil
	default:
		return "", nil, fmt.Errorf("unsupported art url scheme: %s", u.Scheme)
	}
}

// GetPlayerInfo fetches identity, now-playing metadata and control capabilities for a player.
func (m *MPRIS) GetPlayerInfo(bus string) PlayerInfo {
	info := PlayerInfo{
		CanGoNext:     true,
		CanGoPrevious: true,
		CanPlay:       true,
		CanPause:      true,
	}

	if identity, err := m.playerIdentity(bus); err == nil {
		info.Identity = identity
	} else {
		info.Identity = bus
	}

	props, err := m.playerProperties(bus)
	if err != nil {
		return info
	}

	if v, ok := props["Metadata"]; ok {
		var artURL string
		info.Title, info.Artist, info.Album, artURL = parseMetadata(v)
		if mimeType, data, err := fetchArt(artURL); err == nil {
			info.ArtMimeType = mimeType
			info.ArtData = data
		}
	}
	if v, ok := props["PlaybackStatus"]; ok {
		info.PlaybackStatus, _ = v.Value().(string)
	}

	info.CanGoNext = boolPropertyOrTrue(props, "CanGoNext")
	info.CanGoPrevious = boolPropertyOrTrue(props, "CanGoPrevious")
	info.CanPlay = boolPropertyOrTrue(props, "CanPlay")
	info.CanPause = boolPropertyOrTrue(props, "CanPause")

	return info
}

func (m *MPRIS) ListPlayers() (map[string]PlayerInfo, error) {
	players, err := m.mprisPlayers()
	if err != nil {
		return nil, err
	}

	result := make(map[string]PlayerInfo)
	for _, bus := range players {
		result[bus] = m.GetPlayerInfo(bus)
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

func (m *MPRIS) Next() error {
	objs, err := m.allPlayers()
	if err != nil {
		return err
	}
	return call(objs, "Next")
}

func (m *MPRIS) Previous() error {
	objs, err := m.allPlayers()
	if err != nil {
		return err
	}
	return call(objs, "Previous")
}

func (m *MPRIS) PlayPausePlayer(name string) error {
	obj, err := m.object(name, mprisPath)
	if err != nil {
		return err
	}
	return obj.Call(playerIface+".PlayPause", 0).Err
}

func (m *MPRIS) PlayPlayer(name string) error {
	obj, err := m.object(name, mprisPath)
	if err != nil {
		return err
	}
	return obj.Call(playerIface+".Play", 0).Err
}

func (m *MPRIS) PausePlayer(name string) error {
	obj, err := m.object(name, mprisPath)
	if err != nil {
		return err
	}
	return obj.Call(playerIface+".Pause", 0).Err
}

func (m *MPRIS) NextPlayer(name string) error {
	obj, err := m.object(name, mprisPath)
	if err != nil {
		return err
	}
	return obj.Call(playerIface+".Next", 0).Err
}

func (m *MPRIS) PreviousPlayer(name string) error {
	obj, err := m.object(name, mprisPath)
	if err != nil {
		return err
	}
	return obj.Call(playerIface+".Previous", 0).Err
}
