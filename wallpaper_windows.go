package deskconn

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

func (w *Wallpaper) path() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\Desktop`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()

	path, _, err := key.GetStringValue("WallPaper")
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("wallpaper path not found")
	}

	return path, nil
}
