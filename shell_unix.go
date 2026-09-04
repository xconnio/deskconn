//go:build !windows

package deskconn

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

func defaultShell() string {
	return "bash"
}

func foregroundPGIDDiffers(ptmx pty.Pty, pid int) (bool, error) {
	unixPtmx, ok := ptmx.(pty.UnixPty)
	if !ok {
		return false, fmt.Errorf("pty does not support busy detection")
	}

	rawConn, err := unixPtmx.Master().SyscallConn()
	if err != nil {
		return false, fmt.Errorf("failed to access pty: %w", err)
	}

	var fgpgid int
	var ioctlErr error
	if err := rawConn.Control(func(fd uintptr) {
		fgpgid, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	}); err != nil {
		return false, fmt.Errorf("failed to access pty fd: %w", err)
	}
	if ioctlErr != nil {
		return false, fmt.Errorf("failed to get foreground pgid: %w", ioctlErr)
	}

	shellPgid, err := syscall.Getpgid(pid)
	if err != nil {
		return false, fmt.Errorf("failed to get shell pgid: %w", err)
	}

	return fgpgid != shellPgid, nil
}

func watchResize(_ int, onResize func()) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	for range sigChan {
		onResize()
	}
}
