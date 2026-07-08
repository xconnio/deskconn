package deskconn

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/xconnio/xconn-go"
)

type Wallpaper struct {
	conn *dbus.Conn
}

func NewWallpaper(conn *dbus.Conn) *Wallpaper { return &Wallpaper{conn: conn} }

func (w *Wallpaper) readSetting(namespace, key string) (dbus.Variant, error) {
	obj := w.conn.Object("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
	var result dbus.Variant
	err := obj.Call("org.freedesktop.portal.Settings.Read", 0, namespace, key).Store(&result)
	return result, err
}

func (w *Wallpaper) darkModeActive() bool {
	v, err := w.readSetting("org.freedesktop.appearance", "color-scheme")
	if err != nil {
		return false
	}

	val := v.Value()
	inner, ok := val.(dbus.Variant)
	if ok {
		val = inner.Value()
	}
	switch value := val.(type) {
	case uint32:
		return value == 1
	case int32:
		return value == 1
	case int:
		return value == 1
	default:
		return false
	}
}

func (w *Wallpaper) path() (string, error) {
	dark := w.darkModeActive()
	keys := []string{"picture-uri", "picture-uri-dark"}
	if dark {
		keys = []string{"picture-uri-dark", "picture-uri"}
	}

	// 1. XDG portal — works on GNOME 44+ / Ubuntu 24.04+
	for _, key := range keys {
		v, err := w.readSetting("org.gnome.desktop.background", key)
		if err == nil {
			if raw := v.String(); raw != "" {
				if p := fileURIToPath(raw); p != "" {
					return p, nil
				}
			}
		}
	}

	// 2. dconf user database — for user-customized wallpapers
	home, _ := os.UserHomeDir()
	if dconfData, err := os.ReadFile(filepath.Join(home, ".config", "dconf", "user")); err == nil {
		for _, key := range keys {
			if raw := gvdbLookup(dconfData, "/org/gnome/desktop/background/"+key); raw != "" {
				if p := fileURIToPath(raw); p != "" {
					return p, nil
				}
			}
		}
	}

	// 3. GSettings schema override files — for system-default wallpapers
	if p := gsettingsOverrideLookup(keys); p != "" {
		return p, nil
	}

	// 4. MATE
	if v, err := w.readSetting("org.mate.background", "picture-filename"); err == nil {
		if p := v.String(); p != "" {
			return p, nil
		}
	}

	// 5. Cinnamon
	if v, err := w.readSetting("org.cinnamon.desktop.background", "picture-uri"); err == nil {
		if raw := v.String(); raw != "" {
			if p := fileURIToPath(raw); p != "" {
				return p, nil
			}
		}
	}

	// 6. KDE Plasma
	if p := kdeWallpaperPath(); p != "" {
		return p, nil
	}

	return "", fmt.Errorf("wallpaper path not found")
}

func fileURIToPath(raw string) string {
	if strings.HasPrefix(raw, "file://") {
		u, err := url.Parse(raw)
		if err == nil && u.Path != "" {
			return u.Path
		}
	}
	return raw
}

// gvdbLookup reads a string value by key from a dconf binary database.
// Layout: 24-byte file header, then a hash table whose items are each 24 bytes:
// hash[4] parent[4] key_start[4] key_size[2] type[1] _[1] val_start[4] val_end[4].
func gvdbLookup(data []byte, fullKey string) string {
	if len(data) < 24 || string(data[0:8]) != "GVariant" {
		return ""
	}
	if binary.LittleEndian.Uint32(data[8:12]) != 0 { // version 0 = little-endian
		return ""
	}

	ru32 := func(off int) int {
		if off+4 > len(data) {
			return 0
		}
		return int(binary.LittleEndian.Uint32(data[off:]))
	}
	ru16 := func(off int) int {
		if off+2 > len(data) {
			return 0
		}
		return int(binary.LittleEndian.Uint16(data[off:]))
	}

	rootStart := ru32(16)
	rootEnd := ru32(20)
	if rootStart+8 > len(data) || rootEnd > len(data) || rootStart >= rootEnd {
		return ""
	}

	nBuckets := ru32(rootStart + 4)
	itemsStart := rootStart + 8 + nBuckets*4
	itemsEnd := rootEnd
	if itemsStart >= itemsEnd || (itemsEnd-itemsStart)%24 != 0 {
		return ""
	}
	nItems := (itemsEnd - itemsStart) / 24

	getParent := func(i int) int { return ru32(itemsStart + i*24 + 4) }
	getKey := func(i int) []byte {
		off := itemsStart + i*24
		ks := ru32(off + 8)
		klen := ru16(off + 12)
		if ks <= 0 || klen <= 0 || ks+klen > len(data) {
			return nil
		}
		return data[ks : ks+klen]
	}
	getType := func(i int) byte {
		off := itemsStart + i*24 + 14
		if off >= len(data) {
			return 0
		}
		return data[off]
	}
	getVal := func(i int) (int, int) {
		off := itemsStart + i*24
		return ru32(off + 16), ru32(off + 20)
	}

	// Build the full path of item i by chaining parent links.
	var buildPath func(i, depth int) string
	buildPath = func(i, depth int) string {
		if depth > 32 || i < 0 || i >= nItems {
			return ""
		}
		key := getKey(i)
		parent := getParent(i)
		if parent >= nItems { // root item
			return "/" + string(key)
		}
		return buildPath(parent, depth+1) + string(key)
	}

	for i := 0; i < nItems; i++ {
		if getType(i) != 'v' {
			continue
		}
		if buildPath(i, 0) != fullKey {
			continue
		}
		vs, ve := getVal(i)
		if vs > 0 && ve > vs && ve <= len(data) {
			return gvdbExtractString(data[vs:ve])
		}
	}
	return ""
}

// gvdbExtractString parses a GVariant string value.
// Format: [string bytes][NUL][NUL][type "s"].
func gvdbExtractString(variantData []byte) string {
	for i := len(variantData) - 1; i >= 0; i-- {
		if variantData[i] != 0 {
			continue
		}
		if string(variantData[i+1:]) != "s" {
			return ""
		}
		value := variantData[:i]
		if len(value) > 0 && value[len(value)-1] == 0 {
			value = value[:len(value)-1]
		}
		return string(value)
	}
	return ""
}

// gsettingsOverrideLookup reads wallpaper keys from GSettings schema override files.
// This covers cases where the wallpaper is a system default not stored in dconf.
func gsettingsOverrideLookup(keys []string) string {
	files, _ := filepath.Glob("/usr/share/glib-2.0/schemas/*.gschema.override")
	// Two passes: ubuntu-profile sections first (higher priority), then generic.
	for _, wantUbuntu := range []bool{true, false} {
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			inSection := false
			found := map[string]string{}
			lines := strings.Split(string(raw), "\n")
			for idx, line := range lines {
				line = strings.TrimSpace(line)
				newSection := strings.HasPrefix(line, "[") || idx == len(lines)-1
				if newSection && inSection {
					// End of section — return best match found so far.
					for _, key := range keys {
						if v := found[key]; v != "" {
							return fileURIToPath(v)
						}
					}
					found = map[string]string{}
				}
				if strings.HasPrefix(line, "[") {
					header := strings.Trim(line, "[]")
					isBackground := strings.HasPrefix(header, "org.gnome.desktop.background")
					isUbuntu := strings.Contains(header, ":ubuntu")
					isGreeter := strings.Contains(header, "Greeter")
					inSection = isBackground && !isGreeter && (wantUbuntu == isUbuntu)
					continue
				}
				if !inSection {
					continue
				}
				for _, key := range keys {
					if !strings.HasPrefix(line, key) {
						continue
					}
					rest := strings.TrimSpace(strings.TrimPrefix(line, key))
					if !strings.HasPrefix(rest, "=") {
						continue
					}
					val := strings.TrimSpace(rest[1:])
					val = strings.Trim(val, "'\"")
					if val != "" {
						found[key] = val
					}
				}
			}
		}
	}
	return ""
}

func kdeWallpaperPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "plasma-org.kde.plasma.desktop-appletsrc"))
	if err != nil {
		return ""
	}

	inWallpaperSection := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inWallpaperSection = strings.Contains(line, "Wallpaper") && strings.Contains(line, "General")
			continue
		}
		if inWallpaperSection && strings.HasPrefix(line, "Image=") {
			return fileURIToPath(strings.TrimPrefix(line, "Image="))
		}
	}
	return ""
}

func imageMimeTypeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case extPng:
		return "image/png"
	case extWebp:
		return "image/webp"
	case extGif:
		return "image/gif"
	case extBmp:
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	default:
		return "image/jpeg"
	}
}

func (w *Wallpaper) load() (string, []byte, error) {
	path, err := w.path()
	if err != nil {
		return "", nil, err
	}

	data, err := os.ReadFile(path)

	return path, data, err
}

func (w *Wallpaper) HandleGet(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	path, data, err := w.load()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(imageMimeTypeByExt(path), data)
}

func (w *Wallpaper) HandleChecksum(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	_, data, err := w.load()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	sum := sha256.Sum256(data)
	return xconn.NewInvocationResult(hex.EncodeToString(sum[:]))
}
