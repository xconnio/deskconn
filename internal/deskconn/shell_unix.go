//go:build !windows

package deskconn

import (
	"os"
	"os/signal"
	"syscall"
)

func watchResize(_ int, onResize func()) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	for range sigChan {
		onResize()
	}
}
