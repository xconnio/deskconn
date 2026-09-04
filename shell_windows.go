//go:build windows

package deskconn

import (
	"errors"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/term"
)

func defaultShell() string {
	return "powershell.exe"
}

func foregroundPGIDDiffers(_ pty.Pty, _ int) (bool, error) {
	return false, errors.New("busy detection is not supported on windows")
}

func watchResize(fd int, onResize func()) {
	width, height, err := term.GetSize(fd)
	if err != nil {
		return
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		w, h, err := term.GetSize(fd)
		if err != nil {
			continue
		}
		if w != width || h != height {
			width, height = w, h
			onResize()
		}
	}
}
