package userd

import (
	"os/exec"
	"os/user"
	"strings"
)

// accountShell returns the user's login shell from Directory Services: macOS keeps no users
// in /etc/passwd.
func accountShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	// "UserShell: /bin/zsh"
	_, shell, _ := strings.Cut(strings.TrimSpace(string(out)), ":")
	return strings.TrimSpace(shell)
}
