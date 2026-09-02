//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// The vpn subcommand needs the Linux-only iptun package (TUN devices, iptables) - see vpn.go.
// These stubs keep "desk vpn ..." a valid command everywhere, reporting it's unavailable
// here instead of failing to build.

func runVPNConnect(_ context.Context, _, _, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}

func runVPNStart(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}

func runVPNStop(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}

// vpndProgName mirrors vpnd.go's constant of the same name (there, sourced from the
// Linux-only deskconn.VPNHelperName) - the privileged VPN helper itself is Linux-only (see
// vpnd.go, vpn.go), but the literal name still needs to exist here for install/uninstall
// paths and vpndArgs below, which run on every platform.
const vpndProgName = "vpnd"

// vpndArgs mirrors vpnd.go's function of the same name: it always reports false here, since
// runVPNd (below) is never reachable as a real helper backend on this platform, but the check
// itself is trivial and platform-independent, so there's no reason to skip it.
func vpndArgs(argv []string) ([]string, bool) {
	if len(argv) > 0 && filepath.Base(argv[0]) == vpndProgName {
		return argv[1:], true
	}
	return nil, false
}

// runVPNd is unreachable here: vpndArgs above never returns true, since this platform's
// desk binary was never installed with a vpnd symlink pointing at it (see install.sh).
func runVPNd(_ []string) {
	fmt.Fprintln(os.Stderr, "desk: vpn is not supported on this platform")
}
