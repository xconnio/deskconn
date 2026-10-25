//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-core/bindings/go"
	xconnwebrtc "github.com/xconnio/xconn-core/bindings/go/webrtc"
)

// closeSessionWithTimeout closes session with a bound, since Leave()'s
// WAMP GOODBYE round-trip is network I/O that could otherwise make Ctrl-C
// look like it's doing nothing (signal.Notify overrides the terminal's
// default SIGINT-kills behavior while a deferred cleanup step is running).
func closeSessionWithTimeout(session *xconnwebrtc.WebRTCSession) {
	done := make(chan struct{})
	go func() {
		_ = session.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(12 * time.Second):
		fmt.Fprintln(os.Stderr, "desk: timed out closing session cleanly, exiting anyway")
	}
}

const (
	// vpnConnectChildEnv marks the re-executed "desk vpn connect" that
	// actually runs the tunnel (see runVPNConnect), with the write end of a
	// readiness pipe as its fd 3.
	vpnConnectChildEnv = "DESKCONN_VPN_CONNECT_CHILD"

	vpnConnectPIDName = "vpn-connect.pid"
	vpnConnectLogName = "vpn-connect.log"

	// vpnDisconnectTimeout bounds how long "desk vpn disconnect" waits for
	// the tunnel process to finish its teardown: closing the session and
	// waiting for deskconn-rootd to exit are each bounded on their own.
	vpnDisconnectTimeout = 45 * time.Second
)

// runVPNConnect routes this machine's internet traffic through machine and
// returns once the tunnel is up, leaving it running in the background until
// "desk vpn disconnect". Go can't fork, so it re-executes itself (see
// runVPNConnectChild) with the same terminal, so the sudo and any password
// prompt still work, and exits once that child reports the tunnel is up or
// exits itself. The child detaches from the terminal from then on.
func runVPNConnect(cliCtx context.Context, cfgDirectory, realm, machine string) {
	if os.Getenv(vpnConnectChildEnv) != "" {
		runVPNConnectChild(cliCtx, cfgDirectory, realm, machine)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = readyR.Close() }()

	cmd := exec.Command(exe, os.Args[1:]...) //nolint:gosec
	cmd.Env = append(os.Environ(), vpnConnectChildEnv+"=1")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{readyW}

	// Ctrl-C during setup reaches the child too (same process group), which
	// unwinds and exits; just wait for that instead of dying first.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		_ = readyW.Close()
		fmt.Fprintln(os.Stderr, err)
		return
	}
	_ = readyW.Close()

	if n, _ := readyR.Read(make([]byte, 1)); n == 1 {
		return
	}
	_ = cmd.Wait()
}

// runVPNConnectChild runs the tunnel for runVPNConnect until it drops,
// SIGTERM ("desk vpn disconnect") or Ctrl-C before it's up, waiting for its
// own teardown to finish before exiting. This process also launches
// deskconn-rootd (see deskconn.LaunchVPNHelper), since it's the one with a
// terminal for sudo to prompt on, and waits for it to exit.
func runVPNConnectChild(cliCtx context.Context, cfgDirectory, realm, machine string) {
	ready := os.NewFile(3, "vpn-ready")

	release, err := claimVPNConnectPID(cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer release()

	fmt.Printf("Connecting to %q...\n", machine)

	ctx, cancel := context.WithCancel(cliCtx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	session, err := deskconn.ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer closeSessionWithTimeout(session)

	socketPath, waitHelper, err := deskconn.LaunchVPNHelper(ctx, cfgDirectory, deskconn.VPNConnectUnit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() {
		if err := waitHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	helper, err := common.DialVPNHelper(socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = helper.Close() }()

	fmt.Println("Waiting for the remote machine to accept the tunnel...")
	errCh := make(chan error, 1)
	onReady := func() {
		fmt.Printf("Tunnel up: routing this machine's internet traffic through %q.\n", machine)
		fmt.Println("Run \"desk vpn disconnect\" to stop.")
		if err := detachFromTerminal(cfgDirectory); err != nil {
			log.Warnf("vpn: could not detach from terminal: %v", err)
		}
		_, _ = ready.Write([]byte{1})
		_ = ready.Close()
	}
	common.SafeGo(func() { errCh <- deskconn.ConnectVPNClient(ctx, session, helper, onReady) })

	select {
	case <-sigCh:
		cancel()
	case err := <-errCh:
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		fmt.Println("Tunnel down")
		return
	}

	// Stopped: wait for the goroutine above to actually finish (its own
	// teardown included) before letting the process exit.
	if err := <-errCh; err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Println("Tunnel down")
}

// detachFromTerminal moves this process into its own session, off the
// terminal "desk vpn connect" was run from, and points its stdio at a log
// file in cfgDirectory, so it outlives that terminal.
func detachFromTerminal(cfgDirectory string) error {
	logFile, err := os.OpenFile(filepath.Join(cfgDirectory, vpnConnectLogName),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()

	if _, err := unix.Setsid(); err != nil {
		return err
	}
	if err := unix.Dup2(int(devNull.Fd()), 0); err != nil {
		return err
	}
	if err := unix.Dup2(int(logFile.Fd()), 1); err != nil {
		return err
	}
	return unix.Dup2(int(logFile.Fd()), 2)
}

// claimVPNConnectPID records this process as the one running the tunnel,
// for "desk vpn disconnect" to find, refusing if another one already is.
func claimVPNConnectPID(cfgDirectory string) (release func(), err error) {
	if pid, ok := vpnConnectPID(cfgDirectory); ok {
		return nil, fmt.Errorf("already connected (pid %d); run \"desk vpn disconnect\" first", pid)
	}

	path := filepath.Join(cfgDirectory, vpnConnectPIDName)
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(path, []byte(pid), 0o600); err != nil {
		return nil, err
	}
	return func() {
		if data, err := os.ReadFile(path); err == nil && string(data) == pid {
			_ = os.Remove(path)
		}
	}, nil
}

// vpnConnectPID returns the pid of the running "desk vpn connect" tunnel
// process, if there is one.
func vpnConnectPID(cfgDirectory string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(cfgDirectory, vpnConnectPIDName))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// A stale file's pid may since have been reused by something else.
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(cmdline), "vpn\x00connect") {
		return 0, false
	}
	return pid, true
}

// runVPNDisconnect stops the tunnel "desk vpn connect" left running, and
// waits for its teardown to finish. Stopping deskconn-rootd's unit directly
// is only the fallback for one whose tunnel process is already gone.
func runVPNDisconnect(ctx context.Context, cfgDirectory string) {
	pid, ok := vpnConnectPID(cfgDirectory)
	if !ok {
		if !deskconn.VPNHelperActive(deskconn.VPNConnectUnit) {
			fmt.Fprintln(os.Stderr, "not connected")
			return
		}
		if err := deskconn.StopVPNHelper(ctx, deskconn.VPNConnectUnit); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("Disconnected.")
		return
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("Disconnecting...")

	deadline := time.After(vpnDisconnectTimeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, ok := vpnConnectPID(cfgDirectory); !ok {
			fmt.Println("Disconnected.")
			return
		}
		select {
		case <-deadline:
			fmt.Fprintf(os.Stderr, "timed out waiting for the tunnel (pid %d) to stop, see %s\n",
				pid, filepath.Join(cfgDirectory, vpnConnectLogName))
			return
		case <-ticker.C:
		}
	}
}

// runVPNServe arms this machine to let other machines route their traffic
// through it, then returns right away -- serving continues in the
// background (userd + deskconn-rootd) until "desk vpn stop" or the
// daemon shuts down. userd has no capability or terminal of its own
// for this, so this CLI process starts deskconn-rootd as a transient systemd unit
// (the only blocking part, briefly, for the sudo prompt) and hands
// userd its socket. systemd owns deskconn-rootd from there; it exits once
// userd closes its connection.
func runVPNServe(cliCtx context.Context, cfgDirectory string) {
	ctx, cancel := context.WithCancel(cliCtx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			cancel()
		}
	}()

	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
	localSession, err := xconn.ConnectAnonymous(ctx, uri, common.LocalRealm)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = localSession.Leave() }()

	socketPath, _, err := deskconn.LaunchVPNHelper(ctx, cfgDirectory, deskconn.VPNServeUnit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	resp := localSession.Call(common.ProcedureProxyVPNStart).Args(socketPath).ExecuteContext(ctx)
	if resp.Err != nil {
		fmt.Fprintln(os.Stderr, resp.Err)
		return
	}

	fmt.Println("Serving in the background: this machine can now be used as a VPN exit node, " +
		"one connection at a time.")
	fmt.Println("Run \"desk vpn stop\" (from anywhere) to stop.")
}

// runVPNStop tells this machine's own userd to stop serving as a
// VPN exit node, if it currently is -- see runVPNServe. Disarming closes
// userd's connection to deskconn-rootd, which makes it exit without needing
// sudo; stopping the unit directly is only the fallback for a deskconn-rootd that
// userd no longer knows about (e.g. userd isn't running).
func runVPNStop(ctx context.Context, cfgDirectory string) {
	if err := disarmVPNServing(ctx, cfgDirectory); err == nil {
		fmt.Println("Stopped serving.")
		return
	} else if !deskconn.VPNHelperActive(deskconn.VPNServeUnit) {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	if err := deskconn.StopVPNHelper(ctx, deskconn.VPNServeUnit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("Stopped serving.")
}

func disarmVPNServing(ctx context.Context, cfgDirectory string) error {
	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
	localSession, err := xconn.ConnectAnonymous(ctx, uri, common.LocalRealm)
	if err != nil {
		return err
	}
	defer func() { _ = localSession.Leave() }()

	return localSession.Call(common.ProcedureProxyVPNStop).ExecuteContext(ctx).Err
}
