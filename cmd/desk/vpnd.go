//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/deskconn"
)

// vpndProgName is the name desk runs as the privileged VPN helper under: either
// as argv[0] (the installed "vpnd" symlink to desk) or as the first argument
// (how deskconn.LaunchVPNHelper starts it under systemd-run).
const vpndProgName = deskconn.VPNHelperName

// vpndArgs reports whether argv invokes vpnd, and if so returns the
// helper's own arguments. Checked before anything else in main: it runs as
// root, so it must not touch the invoking user's config the way the
// regular CLI does.
func vpndArgs(argv []string) ([]string, bool) {
	if len(argv) > 0 && filepath.Base(argv[0]) == vpndProgName {
		return argv[1:], true
	}
	if len(argv) > 1 && argv[1] == vpndProgName {
		return argv[2:], true
	}
	return nil, false
}

// runVPNd is the privileged backend for "desk vpn": it serves exactly
// one VPN helper client on a unix socket, then exits.
func runVPNd(args []string) {
	app := kingpin.New(vpndProgName, "Privileged backend for desk vpn")
	socketPath := app.Flag("socket", "Unix socket to listen on for the one client this process serves").
		Required().String()
	idleTimeout := app.Flag("idle-timeout", "Exit if no client connects within this long").
		Default("30s").Duration()
	_, err := app.Parse(args)
	app.FatalIfError(err, "")

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "vpnd: must run as root (invoke via sudo)")
		os.Exit(1)
	}

	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unixpacket", *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vpnd: listen on %s: %v\n", *socketPath, err)
		os.Exit(1)
	}
	// Clean up the whole temp dir LaunchVPNHelper made for this socket, not just the socket
	// file -- "desk vpn start" hands off and returns, so this is the only cleanup that dir gets.
	defer func() { _ = os.RemoveAll(filepath.Dir(*socketPath)) }()

	// Only the operator's uid (or root) can reach that dir at all; this just ensures that uid
	// can connect to the socket itself, whatever umask it was started with.
	if err := os.Chmod(*socketPath, 0o666); err != nil { //nolint:gosec
		fmt.Fprintf(os.Stderr, "vpnd: chmod socket: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	// SIGTERM is what "systemctl stop" sends; SIGHUP defensively, since Go's default for it is
	// immediate termination with no cleanup.
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	conn := acceptOne(listener, sigCh, *idleTimeout)
	if conn == nil {
		return
	}

	go func() {
		<-sigCh
		_ = conn.Close()
	}()

	server := &Server{}
	server.Serve(conn)
}

// acceptOne waits for the single client this process will ever serve, or
// gives up (returning nil) on a signal or on *idleTimeout passing with
// nobody connecting -- an orphaned root process that nothing is ever going
// to talk to should not sit around forever.
func acceptOne(listener net.Listener, sigCh <-chan os.Signal, idleTimeout time.Duration) *net.UnixConn {
	defer func() { _ = listener.Close() }()

	acceptCh := make(chan net.Conn, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptCh <- conn
	}()

	select {
	case conn := <-acceptCh:
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			fmt.Fprintln(os.Stderr, "vpnd: unexpected connection type")
			_ = conn.Close()
			return nil
		}
		return unixConn
	case err := <-acceptErrCh:
		fmt.Fprintf(os.Stderr, "vpnd: accept: %v\n", err)
		return nil
	case <-sigCh:
		return nil
	case <-time.After(idleTimeout):
		fmt.Fprintln(os.Stderr, "vpnd: no client connected in time, exiting")
		return nil
	}
}
