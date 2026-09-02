//go:build linux

package deskconn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// VPNHelperName is the program name desk runs as its privileged VPN
	// helper under: main dispatches on it either as argv[0] (the installed
	// symlink) or as the first argument (how LaunchVPNHelper invokes it).
	VPNHelperName = "vpnd"

	// VPNServeUnit is the transient systemd unit vpnd runs as for "desk vpn
	// start", VPNConnectUnit the one for "desk vpn connect". Fixed names,
	// so systemd itself refuses a second instance of either while one is
	// running, and so "systemctl is-active" tells whether one is.
	VPNServeUnit   = "deskconn-vpnd"
	VPNConnectUnit = "deskconn-vpnd-connect"

	// helperReadyTimeout bounds how long we wait for the helper's socket to
	// come up once systemd-run has returned -- the sudo password has already
	// been entered by then, so this only covers process startup.
	helperReadyTimeout = 15 * time.Second
	helperPollInterval = 200 * time.Millisecond

	// helperStopTimeout bounds how long wait waits for the unit to finish
	// unwinding after its client connection closes.
	helperStopTimeout = 15 * time.Second
)

// LaunchVPNHelper starts vpnd (this same executable) as the transient
// systemd unit named unit, via "sudo systemd-run" -- prompting for a
// password on this process's terminal -- and waits for its socket to come
// up. Connect to the returned path with DialVPNHelper, from this process or
// (serve mode) handed to deskconnd to dial instead; the helper serves
// exactly one connection and unwinds and exits once it closes, at which
// point systemd discards the unit.
//
// wait blocks until the unit has actually exited (bounded), so a caller
// that closed its connection can tell the helper finished its teardown.
// Callers that hand the helper off and return don't need to call it.
func LaunchVPNHelper(ctx context.Context, cfgDirectory, unit string) (socketPath string, wait func() error, err error) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "", nil, fmt.Errorf("%s needs systemd: systemd-run not found", VPNHelperName)
	}
	if VPNHelperActive(unit) {
		return "", nil, fmt.Errorf("%s is already running (unit %s)", VPNHelperName, unit)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("find own executable for %s: %w", VPNHelperName, err)
	}

	// The socket lives in a private dir under the operator's own config dir, so only their
	// uid (or root) can reach it; vpnd removes the dir when it exits.
	dir, err := os.MkdirTemp(cfgDirectory, "vpnhelper-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir for vpn helper socket: %w", err)
	}
	socketPath = filepath.Join(dir, "helper.sock")

	fmt.Println("A password is needed to grant vpnd the network access this requires.")
	cmd := exec.CommandContext(ctx, "sudo", "systemd-run", //nolint:gosec
		"--unit="+unit,
		"--description=deskconn VPN helper",
		"--collect", // discard the unit on exit even if it failed, so the name is free again
		"--quiet",
		"--", exe, VPNHelperName, "--socket", socketPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("start %s: %w", VPNHelperName, err)
	}

	if err := waitForSocket(ctx, socketPath, unit); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}

	wait = func() error { return waitForUnitExit(unit) }
	return socketPath, wait, nil
}

// VPNHelperActive reports whether the systemd unit named unit is currently
// running. Needs no privileges.
func VPNHelperActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil //nolint:gosec
}

// StopVPNHelper stops the systemd unit named unit via sudo, for when it
// can't be wound down by closing its client connection instead.
func StopVPNHelper(ctx context.Context, unit string) error {
	cmd := exec.CommandContext(ctx, "sudo", "systemctl", "stop", unit) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("stop %s: %w", unit, err)
	}
	return nil
}

func waitForSocket(ctx context.Context, socketPath, unit string) error {
	deadline := time.After(helperReadyTimeout)
	ticker := time.NewTicker(helperPollInterval)
	defer ticker.Stop()

	for {
		if info, statErr := os.Stat(socketPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if !VPNHelperActive(unit) {
			return fmt.Errorf("%s exited before it was ready (see \"journalctl -u %s\")", VPNHelperName, unit)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for %s to start (see \"journalctl -u %s\")", VPNHelperName, unit)
		case <-ticker.C:
		}
	}
}

func waitForUnitExit(unit string) error {
	deadline := time.After(helperStopTimeout)
	ticker := time.NewTicker(helperPollInterval)
	defer ticker.Stop()

	for VPNHelperActive(unit) {
		select {
		case <-deadline:
			return fmt.Errorf("timed out waiting for %s to exit (non-fatal)", VPNHelperName)
		case <-ticker.C:
		}
	}
	return nil
}
