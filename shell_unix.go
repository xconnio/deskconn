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

// killShellProcessGroup forcibly terminates a shell/exec's whole process
// group (the spawned command plus anything it started, e.g. background
// jobs in an interactive shell). Closing the pty alone is not a reliable way
// to do this: the kernel is supposed to deliver SIGHUP to the foreground
// process group when a PTY's master side closes, but that can race with
// startOutputReader's own concurrent blocked Read on the same file and --
// empirically -- silently fail to happen at all, leaving the process
// running forever. pid <= 0 is a no-op (never valid, just defensive). This
// relies on go-pty's Cmd setting Setsid on Unix (cmd_unix.go), which makes
// the child its own process group leader, so -pid reaches the whole group.
func killShellProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
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
