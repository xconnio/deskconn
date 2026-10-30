//go:build linux || darwin

package deskconn

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls onResize each time the terminal is resized, until done is closed.
func watchResize(_ int, done <-chan struct{}, onResize func()) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)
	for {
		select {
		case <-done:
			return
		case <-sigChan:
			onResize()
		}
	}
}
