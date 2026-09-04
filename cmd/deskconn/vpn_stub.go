//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
)

// The vpn subcommand needs the Linux-only iptun package (TUN devices, iptables) - see vpn.go.
// These stubs keep "deskconn vpn ..." a valid command everywhere, reporting it's unavailable
// here instead of failing to build.

func runVPNConnect(_ context.Context, _, _, _ string) {
	fmt.Fprintln(os.Stderr, "deskconn: vpn is not supported on this platform")
}

func runVPNStart(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "deskconn: vpn is not supported on this platform")
}

func runVPNStop(_ context.Context, _ string) {
	fmt.Fprintln(os.Stderr, "deskconn: vpn is not supported on this platform")
}
