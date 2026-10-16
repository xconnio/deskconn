//go:build unix && !darwin

package userd

import (
	"os/exec"
	"os/user"
	"strings"
)

// accountShell returns the user's login shell from the user database (/etc/passwd, LDAP, ...).
func accountShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	out, err := exec.Command("getent", "passwd", u.Username).Output() //nolint:gosec
	if err != nil {
		return ""
	}
	// name:password:uid:gid:gecos:home:shell
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	return fields[len(fields)-1]
}
