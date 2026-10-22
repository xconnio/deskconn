//go:build windows

package deskconn

import (
	"time"

	"golang.org/x/term"
)

// watchResize calls onResize each time the terminal is resized, until done is closed.
func watchResize(fd int, done <-chan struct{}, onResize func()) {
	width, height, err := term.GetSize(fd)
	if err != nil {
		return
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}
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
