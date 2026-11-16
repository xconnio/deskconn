//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/xconnio/deskconn/internal/common"
)

var errVPNUnsupported = errors.New("desk: vpn is not supported on this platform")

// The vpn subcommand and the root helper it talks to are both Linux-only (TUN devices,
// iptables) - see vpn.go, rootd.go and the rootd package. These stubs keep "desk vpn ..."
// and desk serve --mode root valid everywhere, reporting them as unavailable
// here instead of failing to build.

func runRootd(_ rootdFlags) {
	fmt.Fprintln(os.Stderr, "desk: the root helper is not supported on this platform")
	os.Exit(exitFailure)
}

func runVPNConnect(_ context.Context, _, _, _ string, _ common.VPNRoutes, _ bool) error {
	return errVPNUnsupported
}

func runVPNServe(_ context.Context, _ string) error {
	return errVPNUnsupported
}

func runVPNStop(_ context.Context, _ string) error {
	return errVPNUnsupported
}

func runVPNDisconnect(_ context.Context, _ string) error {
	return errVPNUnsupported
}
