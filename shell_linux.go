//go:build linux

package deskconn

import (
	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

// disablePTYEcho turns off ptmx's line discipline echoing (see
// startPtySession's doc comment for why). Best-effort: if the ioctl fails
// there's nothing more useful to do than leave the default in place.
//
// TCGETS/TCSETS are Linux's termios ioctl request names; Darwin/BSD use
// TIOCGETA/TIOCSETA instead for the same operation (see shell_darwin.go).
func disablePTYEcho(ptmx pty.Pty) {
	termios, err := unix.IoctlGetTermios(int(ptmx.Fd()), unix.TCGETS)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO | unix.ECHOCTL | unix.ECHOE | unix.ECHOK | unix.ECHONL
	_ = unix.IoctlSetTermios(int(ptmx.Fd()), unix.TCSETS, termios)
}
