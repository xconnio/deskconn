package main

import "path/filepath"

// multicallArgs reports whether argv invokes the deskconn binary as name, and if so
// returns that program's own arguments: name is either argv[0] (an installed symlink to
// the binary) or the first argument (how deskconn.LaunchVPNHelper starts deskconn-rootd,
// and how "go run" can start either daemon).
func multicallArgs(argv []string, name string) ([]string, bool) {
	if len(argv) > 0 && filepath.Base(argv[0]) == name {
		return argv[1:], true
	}
	if len(argv) > 1 && argv[1] == name {
		return argv[2:], true
	}
	return nil, false
}
