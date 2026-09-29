package deskconn

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
	// VPNHelperName is the program name deskconn runs as its privileged VPN
	// helper under: main dispatches on argv[0], snap-style, and
	// LaunchVPNHelper invokes deskconn through a symlink by this name.
	VPNHelperName = "vpnd"

	// helperReadyTimeout bounds how long we wait for the helper's socket to
	// come up -- generous, since it covers the operator actually typing
	// their sudo password, not just process startup.
	helperReadyTimeout = 2 * time.Minute
	helperPollInterval = 200 * time.Millisecond
)

// LaunchVPNHelper starts vpnd (this same executable, run through a
// symlink named VPNHelperName) under sudo -- prompting for a password
// on this process's terminal, once per tunnel -- and waits for its socket
// to come up. Connect to the returned path with DialClient, from this
// process or (proxy mode) handed to deskconnd to dial instead; the helper
// serves exactly one connection and unwinds once it closes.
//
// vpnd re-execs itself into a detached child and exits almost
// immediately (see detachToNewSession), so wait mostly just reaps that
// launcher and cleans up the temp dir -- it's not a signal that the
// detached helper has actually finished; that safety comes from awaited
// RPCs before a caller closes its connection, not from this. Call wait
// from a defer, after the tunnel is done.
func LaunchVPNHelper(ctx context.Context, cfgDirectory string) (socketPath string, wait func() error, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("find own executable for %s: %w", VPNHelperName, err)
	}

	dir, err := os.MkdirTemp(cfgDirectory, "vpnhelper-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir for vpn helper socket: %w", err)
	}
	socketPath = filepath.Join(dir, "helper.sock")

	// sudo passes the path it's given through as argv[0], so the symlink's name is what
	// tells deskconn to run as the helper. It lives in this launch's private dir, so it
	// needs nothing installed and goes away with the dir.
	helperPath := filepath.Join(dir, VPNHelperName)
	if err := os.Symlink(exe, helperPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("link %s: %w", VPNHelperName, err)
	}

	cmd := exec.Command("sudo", helperPath, "--socket", socketPath) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("start %s: %w", VPNHelperName, err)
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
		// doesn't affect the detached vpnd child either way.
		const reapTimeout = 10 * time.Second
		select {
		case <-exited.done:
			_ = os.RemoveAll(dir)
			var exitErr *exec.ExitError
			if exited.err != nil && !errors.As(exited.err, &exitErr) {
				return fmt.Errorf("wait for %s: %w", VPNHelperName, exited.err)
			}
			return nil
		case <-time.After(reapTimeout):
			_ = os.RemoveAll(dir)
			return fmt.Errorf("timed out reaping %s launcher process (non-fatal)", VPNHelperName)
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
				return fmt.Errorf("%s exited before it was ready: %w", VPNHelperName, exited.err)
			}
			exitedDone = nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for %s to start (sudo password not entered in time?)", VPNHelperName)
		case <-ticker.C:
		}
	}
}
