//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/deskconn/iptun"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

// launchHelper wraps iptun.LaunchHelper with a heads-up that a password
// prompt is coming -- called only once whatever this command needs is
// confirmed reachable, so a failure fails fast without prompting first.
func launchHelper(ctx context.Context, cfgDirectory string) (string, func() error, error) {
	fmt.Println("A password is needed to grant deskconn-vpnd the network access this requires.")
	return iptun.LaunchHelper(ctx, cfgDirectory)
}

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
		fmt.Fprintln(os.Stderr, "deskconn: timed out closing session cleanly, exiting anyway")
	}
}

// runVPNConnect routes this machine's internet traffic through device: it
// dials it directly (P2P) and runs the tunnel here, so on Ctrl-C we wait
// for its own teardown to actually finish before exiting. This process
// also launches deskconn-vpnd (see iptun.LaunchHelper), since it's the one
// with a terminal for sudo to prompt on.
func runVPNConnect(cliCtx context.Context, cfgDirectory, realm, device string) {
	fmt.Printf("Connecting to %q...\n", device)

	ctx, cancel := context.WithCancel(cliCtx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	session, err := deskconn.ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer closeSessionWithTimeout(session)

	socketPath, waitHelper, err := launchHelper(ctx, cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() {
		if err := waitHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	helper, err := iptun.DialClient(socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = helper.Close() }()

	fmt.Println("Waiting for the remote device to accept the tunnel...")
	errCh := make(chan error, 1)
	onReady := func() {
		fmt.Printf("Tunnel up: routing this machine's internet traffic through %q. Press Ctrl-C to stop.\n", device)
	}
	deskconn.SafeGo(func() { errCh <- deskconn.ConnectVPNClient(ctx, session, helper, onReady) })

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

	// Ctrl-C: wait for the goroutine above to actually finish (its own
	// teardown included) before letting the process exit.
	if err := <-errCh; err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Println("Tunnel down")
}

// runVPNStart arms this machine to let other devices route their traffic
// through it, then returns right away -- serving continues in the
// background (deskconnd + deskconn-vpnd) until "deskconn vpn stop" or the
// daemon shuts down. deskconnd has no capability or terminal of its own
// for this, so this CLI process launches deskconn-vpnd (the only blocking
// part, briefly, for the sudo prompt) and hands deskconnd its socket.
//
// deskconn-vpnd is deliberately left running, not reaped here -- it cleans
// up its own temp directory on exit regardless of how it's later stopped.
func runVPNStart(cliCtx context.Context, cfgDirectory string) {
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
	localSession, err := xconn.ConnectAnonymous(ctx, uri, deskconn.LocalRealm)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = localSession.Leave() }()

	socketPath, _, err := launchHelper(ctx, cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	resp := localSession.Call(deskconn.ProcedureProxyVPNStart).Args(socketPath).DoContext(ctx)
	if resp.Err != nil {
		fmt.Fprintln(os.Stderr, resp.Err)
		return
	}

	fmt.Println("Serving in the background: this machine can now be used as a VPN exit node, " +
		"one connection at a time.")
	fmt.Println("Run \"deskconn vpn stop\" (from anywhere) to stop.")
}

// runVPNStop tells this machine's own deskconnd to stop serving as a
// VPN exit node, if it currently is -- see runVPNStart.
func runVPNStop(ctx context.Context, cfgDirectory string) {
	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
	localSession, err := xconn.ConnectAnonymous(ctx, uri, deskconn.LocalRealm)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = localSession.Leave() }()

	resp := localSession.Call(deskconn.ProcedureProxyVPNStop).DoContext(ctx)
	if resp.Err != nil {
		fmt.Fprintln(os.Stderr, resp.Err)
		return
	}
	fmt.Println("Stopped serving.")
}
