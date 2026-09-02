//go:build darwin

package deskconn

import (
	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/unix"
)

// disablePTYEcho turns off ptmx's line discipline echoing (see
// startPtySession's doc comment for why). Best-effort: if the ioctl fails
// there's nothing more useful to do than leave the default in place.
//
// TIOCGETA/TIOCSETA are Darwin/BSD's termios ioctl request names; Linux uses
// TCGETS/TCSETS instead for the same operation (see shell_linux.go).
func disablePTYEcho(ptmx pty.Pty) {
	termios, err := unix.IoctlGetTermios(int(ptmx.Fd()), unix.TIOCGETA)
	if err != nil {
		return
	}
	termios.Lflag &^= unix.ECHO | unix.ECHOCTL | unix.ECHOE | unix.ECHOK | unix.ECHONL
	_ = unix.IoctlSetTermios(int(ptmx.Fd()), unix.TIOCSETA, termios)
}
