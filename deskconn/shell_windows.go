//go:build windows

package deskconn

import (
	"time"

	"golang.org/x/term"
)

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
