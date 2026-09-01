package iptun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	helperBinaryName = "deskconn-vpnd"

	// helperReadyTimeout bounds how long we wait for the helper's socket to
	// come up -- generous, since it covers the operator actually typing
	// their sudo password, not just process startup.
	helperReadyTimeout = 2 * time.Minute
	helperPollInterval = 200 * time.Millisecond
)

// LaunchHelper starts deskconn-vpnd under sudo -- prompting for a password
// on this process's terminal, once per tunnel -- and waits for its socket
// to come up. Connect to the returned path with DialClient, from this
// process or (proxy mode) handed to deskconnd to dial instead; the helper
// serves exactly one connection and unwinds once it closes.
//
// deskconn-vpnd re-execs itself into a detached child and exits almost
// immediately (see detachToNewSession), so wait mostly just reaps that
// launcher and cleans up the temp dir -- it's not a signal that the
// detached helper has actually finished; that safety comes from awaited
// RPCs before a caller closes its connection, not from this. Call wait
// from a defer, after the tunnel is done.
func LaunchHelper(ctx context.Context, cfgDirectory string) (socketPath string, wait func() error, err error) {
	helperPath, err := findHelperBinary()
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp(cfgDirectory, "vpnhelper-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir for vpn helper socket: %w", err)
	}
	socketPath = filepath.Join(dir, "helper.sock")

	cmd := exec.Command("sudo", helperPath, "--socket", socketPath) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("start %s: %w", helperBinaryName, err)
	}

	// procExit's done is closed, not sent-to, so both waitForSocket and wait can each read the
	// exit independently -- a plain "chan error" would let whichever reads first (usually
	// waitForSocket, since the launcher now exits almost immediately) starve the other.
	exited := &procExit{done: make(chan struct{})}
	go func() {
		exited.err = cmd.Wait()
		close(exited.done)
	}()

	if err := waitForSocket(ctx, socketPath, exited); err != nil {
		_ = cmd.Process.Kill()
		<-exited.done
		_ = os.RemoveAll(dir)
		return "", nil, err
	}

	wait = func() error {
		// reapTimeout is just a safety net in case the launcher is somehow still running; it
		// doesn't affect the detached deskconn-vpnd child either way.
		const reapTimeout = 10 * time.Second
		select {
		case <-exited.done:
			_ = os.RemoveAll(dir)
			var exitErr *exec.ExitError
			if exited.err != nil && !errors.As(exited.err, &exitErr) {
				return fmt.Errorf("wait for %s: %w", helperBinaryName, exited.err)
			}
			return nil
		case <-time.After(reapTimeout):
			_ = os.RemoveAll(dir)
			return fmt.Errorf("timed out reaping %s launcher process (non-fatal)", helperBinaryName)
		}
	}
	return socketPath, wait, nil
}

// procExit reports an exec.Cmd's exit to multiple independent readers: done
// is closed once err is safe to read, so any number of callers can select
// on it repeatedly (unlike a plain "chan error", readable only once).
type procExit struct {
	done chan struct{}
	err  error
}

func waitForSocket(ctx context.Context, socketPath string, exited *procExit) error {
	deadline := time.After(helperReadyTimeout)
	ticker := time.NewTicker(helperPollInterval)
	defer ticker.Stop()

	// Stop selecting on exited.done once observed once -- a closed channel is always ready, so
	// leaving it in would busy-loop instead of waiting on ticker.
	exitedDone := exited.done
	for {
		if info, statErr := os.Stat(socketPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}

		select {
		case <-exitedDone:
			// A clean exit here just means the launcher re-exec'd and handed off (see
			// detachToNewSession) -- keep polling. Only a non-zero exit is an actual failure.
			if exited.err != nil {
				return fmt.Errorf("%s exited before it was ready: %w", helperBinaryName, exited.err)
			}
			exitedDone = nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for %s to start (sudo password not entered in time?)", helperBinaryName)
		case <-ticker.C:
		}
	}
}

// findHelperBinary looks for deskconn-vpnd next to this process's own
// executable first (how install.sh lays binaries out), falling back to
// PATH.
func findHelperBinary() (string, error) {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), helperBinaryName)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	if path, err := exec.LookPath(helperBinaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("%s not found (expected next to this binary or on PATH); "+
		"is deskconn installed via install.sh?", helperBinaryName)
}
