//go:build windows

package deskconnd

import (
	"errors"
	"os"

	pty "github.com/aymanbagabas/go-pty"
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
