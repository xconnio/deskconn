//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
)

// The vpn subcommand and the root helper it talks to both need the Linux-only iptun
// package (TUN devices, iptables) - see vpn.go, rootd.go. These stubs keep "desk vpn ..."
// and the deskconn-rootd multicall entry valid everywhere, reporting them as unavailable
// here instead of failing to build.

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
