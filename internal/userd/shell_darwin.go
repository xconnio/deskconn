package userd

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// defaultShell is zsh, macOS's default login shell since Catalina.
func defaultShell() string {
	return "zsh"
}
