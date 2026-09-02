//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// The vpn subcommand and the root helper it talks to both need the Linux-only iptun
// package (TUN devices, iptables) - see vpn.go, rootd.go. These stubs keep "desk vpn ..."
// and the deskconn-rootd multicall entry valid everywhere, reporting them as unavailable
// here instead of failing to build.

// multicallArgs mirrors rootd.go's function of the same name: it has no Linux-specific
// logic at all, but lives there, so it needs a copy here too for main.go to call on every
// platform.
func multicallArgs(argv []string, name string) ([]string, bool) {
	if len(argv) > 0 && filepath.Base(argv[0]) == name {
		return argv[1:], true
	}
	if len(argv) > 1 && argv[1] == name {
		return argv[2:], true
	}
	return nil, false
}

func runRootd(_ []string) {
	fmt.Fprintln(os.Stderr, "deskconn: the root helper is not supported on this platform")
}

func runVPNConnect(_ context.Context, _, _, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}

func runVPNServe(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}

func runVPNStop(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}
