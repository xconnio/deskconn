//go:build linux

package deskconn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// VPNServeUnit is the transient systemd unit deskconn-rootd runs as for "desk vpn
	// serve", VPNConnectUnit the one for "desk vpn connect". Fixed names,
	// so systemd itself refuses a second instance of either while one is
	// running, and so "systemctl is-active" tells whether one is.
	VPNServeUnit   = RootdName
	VPNConnectUnit = RootdName + "-connect"

	// helperReadyTimeout bounds how long we wait for the helper's socket to
	// come up once systemd-run has returned -- the sudo password has already
	// been entered by then, so this only covers process startup.
	helperReadyTimeout = 15 * time.Second
	helperPollInterval = 200 * time.Millisecond

	// helperStopTimeout bounds how long wait waits for the unit to finish
	// unwinding after its client connection closes.
	helperStopTimeout = 15 * time.Second

	// RootdRunDir holds, without systemd, the pid file and log of each running deskconn-rootd
	// (see RootdPIDFile), in place of the unit systemd would otherwise keep track of.
	RootdRunDir = "/run/" + RootdName
)

// RootdPIDFile is where deskconn-rootd, started without systemd for unit, records its pid.
func RootdPIDFile(unit string) string {
	return filepath.Join(RootdRunDir, unit+".pid")
}

// RootdLogFile is where deskconn-rootd, started without systemd for unit, writes its output.
func RootdLogFile(unit string) string {
	return filepath.Join(RootdRunDir, unit+".log")
}

// systemdRunning reports whether systemd is this machine's init (the check sd_booted makes),
// so that systemd-run can actually start units -- installed isn't enough, e.g. in a
// container or under WSL without systemd, or on Devuan, Alpine, Void, Artix...
func systemdRunning() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemd-run")
	return err == nil
}

// privilegeCommand returns the command for running something as root: sudo, or doas where
// that's what the distro uses instead (e.g. Alpine, OpenBSD-minded setups).
func privilegeCommand() (string, error) {
	for _, name := range []string{"sudo", "doas"} {
		if _, err := exec.LookPath(name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s needs root: neither sudo nor doas found", RootdName)
}

// LaunchVPNHelper starts deskconn-rootd (this same executable, as "desk serve --mode
// root") as the transient systemd unit named unit, via "sudo systemd-run" -- prompting for a
// password on this process's terminal -- and waits for its socket to come
// up. Without systemd it runs "sudo desk serve --mode root --detach" (or doas) instead,
// which puts itself in the background and tracks itself by pid file under RootdRunDir.
// Connect to the returned path with DialVPNHelper, from this process or
// (serve mode) handed to userd to dial instead; the helper serves
// exactly one connection and unwinds and exits once it closes, at which
// point systemd discards the unit.
//
// wait blocks until the unit has actually exited (bounded), so a caller
// that closed its connection can tell the helper finished its teardown.
// Callers that hand the helper off and return don't need to call it.
func LaunchVPNHelper(ctx context.Context, cfgDirectory, unit string) (socketPath string, wait func() error, err error) {
	priv, err := privilegeCommand()
	if err != nil {
		return "", nil, err
	}
	if VPNHelperActive(unit) {
		return "", nil, fmt.Errorf("%s is already running (unit %s)", RootdName, unit)
	}

	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("find own executable for %s: %w", RootdName, err)
	}

	// The socket lives in a private dir under the operator's own config dir, so only their
	// uid (or root) can reach it; deskconn-rootd removes the dir when it exits.
	dir, err := os.MkdirTemp(cfgDirectory, "vpnhelper-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir for vpn helper socket: %w", err)
	}
	socketPath = filepath.Join(dir, "helper.sock")

	fmt.Printf("A password is needed to grant %s the network access this requires.\n", RootdName)
	var args []string
	if systemdRunning() {
		args = []string{"systemd-run",
			"--unit=" + unit,
			"--description=deskconn VPN helper",
			"--collect", // discard the unit on exit even if it failed, so the name is free again
			"--quiet",
			"--", exe, "serve", "--mode", "root", "--socket", socketPath}
	} else {
		args = []string{exe, "serve", "--mode", "root", "--socket", socketPath,
			"--detach", "--pid-file", RootdPIDFile(unit)}
	}
	cmd := exec.CommandContext(ctx, priv, args...) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("start %s: %w", RootdName, err)
	}

	if err := waitForSocket(ctx, socketPath, unit); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}

	wait = func() error { return waitForUnitExit(unit) }
	return socketPath, wait, nil
}

// VPNHelperActive reports whether deskconn-rootd is currently running as unit (see
// LaunchVPNHelper). Needs no privileges.
func VPNHelperActive(unit string) bool {
	if !systemdRunning() {
		_, ok := runningRootdPID(unit)
		return ok
	}
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil //nolint:gosec
}

// runningRootdPID returns the pid in unit's pid file, if that process is still a
// deskconn-rootd. It runs as root, so signal 0 answers EPERM for "exists"; its command line
// is checked too, as a stale pid file's pid may have been reused since.
func runningRootdPID(unit string) (int, bool) {
	data, err := os.ReadFile(RootdPIDFile(unit))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0, false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err == nil && !bytes.Contains(cmdline, []byte("--mode\x00root")) {
		return 0, false
	}
	return pid, true
}

// helperLogHint says where to look for why deskconn-rootd running as unit failed.
func helperLogHint(unit string) string {
	if !systemdRunning() {
		return RootdLogFile(unit)
	}
	return fmt.Sprintf("\"journalctl -u %s\"", unit)
}

// StopVPNHelper stops deskconn-rootd running as unit via sudo, for when it
// can't be wound down by closing its client connection instead.
func StopVPNHelper(ctx context.Context, unit string) error {
	priv, err := privilegeCommand()
	if err != nil {
		return err
	}
	args := []string{"systemctl", "stop", unit}
	if !systemdRunning() {
		pid, ok := runningRootdPID(unit)
		if !ok {
			return nil
		}
		args = []string{"kill", strconv.Itoa(pid)}
	}
	cmd := exec.CommandContext(ctx, priv, args...) //nolint:gosec
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
			return fmt.Errorf("%s exited before it was ready (see %s)", RootdName, helperLogHint(unit))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for %s to start (see %s)", RootdName, helperLogHint(unit))
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
			return fmt.Errorf("timed out waiting for %s to exit (non-fatal)", RootdName)
		case <-ticker.C:
		}
	}
	return nil
}
