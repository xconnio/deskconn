package deskconn

import (
	"path/filepath"
	"strings"
)

// IsRemotePath reports whether s names a path on a machine, machine:path, rather than a
// local one; a Windows drive letter isn't a machine.
func IsRemotePath(s string) bool {
	if filepath.IsAbs(s) && filepath.VolumeName(s) != "" {
		return false
	}
	return strings.ContainsRune(s, ':')
}

// ParseMachinePath splits machine:path; without a colon, s is all machine.
func ParseMachinePath(s string) (machine, path string) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}
