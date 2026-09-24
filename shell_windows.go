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

// disablePTYEcho is a no-op here: echoing in a ConPTY session is done by the console app
// running inside it (e.g. PowerShell's own line editor), not by a termios-like driver layer
// the host process can toggle from outside like on *nix -- see startPtySession's doc comment
// for what this is meant to suppress. exec commands may see a stray echoed control byte in
// their output on Windows as a result; there's no ConPTY API to prevent it.
func disablePTYEcho(pty.Pty) {}

// closePtyOnProcessExit closes ptmx once the child has exited. Unlike a Unix
// pty, ConPTY does not signal EOF on its output pipe when the attached
// process exits on its own -- a ConPTY, like a real console host, is
// designed to outlive any single process run inside it -- so
// startOutputReader's blocked Read would otherwise never return for a
// one-shot exec'd command, only for an interactive shell the user
// explicitly exits. Forcing the close here is what makes both behave the
// same way: the pty's lifetime always ends with its process's. Goes through
// ps.closePty rather than ptmx.Close() directly since startOutputReader's
// own cleanup closes the same ptmx too -- double-closing a ConPTY handle is
// undefined behavior, unlike on *nix.
func closePtyOnProcessExit(ps *ptySession, ptmx pty.Pty) {
	ps.closePty(ptmx)
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
