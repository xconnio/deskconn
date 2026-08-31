package deskconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
