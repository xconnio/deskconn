package userd

import (
	"os/exec"
	"os/user"
	"strings"
)

// accountShell returns the user's login shell from Directory Services, macOS's user database.
func accountShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	// Output is "UserShell: /bin/zsh".
	_, shell, found := strings.Cut(strings.TrimSpace(string(out)), ": ")
	if !found {
		return ""
	}
	return shell
}
