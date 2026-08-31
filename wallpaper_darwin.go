package deskconn

import (
	"fmt"
	"os/exec"
	"strings"
)

func (w *Wallpaper) path() (string, error) {
	script := `tell application "System Events" to get picture of current desktop`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}

	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("wallpaper path not found")
	}

	return path, nil
}
