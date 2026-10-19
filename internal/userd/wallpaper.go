package userd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
)

type Wallpaper struct {
	conn *dbus.Conn
}

func NewWallpaper(conn *dbus.Conn) *Wallpaper { return &Wallpaper{conn: conn} }

func fileURIToPath(raw string) string {
	if strings.HasPrefix(raw, "file://") {
		u, err := url.Parse(raw)
		if err == nil && u.Path != "" {
			return u.Path
		}
	}
	return raw
}

func imageMimeTypeByExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case common.ExtPng:
		return "image/png"
	case common.ExtWebp:
		return "image/webp"
	case common.ExtGif:
		return "image/gif"
	case common.ExtBmp:
		return "image/bmp"
	case extSVG:
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

func (d *Deskconn) handleWallpaperGet(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	path, data, err := d.wallpaper.load()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return encryptResult(enc, imageResult{Mime: imageMimeTypeByExt(path), Data: data})
}

func (d *Deskconn) handleWallpaperChecksum(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}

	_, data, err := d.wallpaper.load()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	sum := sha256.Sum256(data)
	return encryptResult(enc, hex.EncodeToString(sum[:]))
}
