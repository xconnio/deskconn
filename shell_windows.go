//go:build windows

package deskconn

import (
	"errors"
	"os"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/term"
)

func defaultShell() string {
	return "powershell.exe"
}

// killShellProcessGroup terminates the shell/exec child. Unlike the Unix
// implementation (shell_unix.go), this can't reach the whole process group --
// go-pty's Windows Cmd never sets up a job object linking child processes
// together, so background jobs a shell started are not reachable here; this
// is a best-effort kill of the immediate child only.
func killShellProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
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
