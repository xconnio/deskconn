//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/deskconn"
)

const (
	// reexecEnvVar marks a process as already having gone through detachToNewSession, so it
	// doesn't re-exec itself again -- see that function.
	reexecEnvVar = "DESKCONN_VPND_DETACHED"

	// vpndProgName is the program name deskconn runs as the privileged VPN helper under,
	// snap-style: installed as a "vpnd" symlink to deskconn, and invoked through a symlink
	// by this name by deskconn.LaunchVPNHelper.
	vpndProgName = deskconn.VPNHelperName
)

// vpndArgs reports whether argv[0] names vpnd, and if so returns the
// helper's own arguments. Checked before anything else in main: it runs as
// root under sudo, so it must not touch the invoking user's config the way
// the regular CLI does.
func vpndArgs(argv []string) ([]string, bool) {
	if len(argv) > 0 && filepath.Base(argv[0]) == vpndProgName {
		return argv[1:], true
	}
	return nil, false
}

// runVPNd is the privileged backend for "deskconn vpn": it serves exactly
// one VPN helper client on a unix socket, then exits.
func runVPNd(args []string) {
	app := kingpin.New(vpndProgName, "Privileged backend for deskconn vpn")
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

	if os.Getenv(reexecEnvVar) == "" {
		if err := detachToNewSession(args); err != nil {
			fmt.Fprintf(os.Stderr, "vpnd: could not detach from terminal, continuing anyway: %v\n", err)
		} else {
			return // the re-exec'd child takes over from here
		}
	}

	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unixpacket", *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vpnd: listen on %s: %v\n", *socketPath, err)
		os.Exit(1)
	}
	// Clean up the whole temp dir LaunchHelper made for this socket, not just the socket file
	// -- "deskconn vpn start" hands off and never reaps us itself, so this is the only cleanup
	// that dir gets.
	defer func() { _ = os.RemoveAll(filepath.Dir(*socketPath)) }()

	// Only the operator's uid (or root) can reach that dir at all; this just ensures that uid
	// can connect to the socket itself, whatever umask sudo left it with.
	if err := os.Chmod(*socketPath, 0o666); err != nil { //nolint:gosec
		fmt.Fprintf(os.Stderr, "vpnd: chmod socket: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	// SIGHUP defensively: Go's default for it is immediate termination with no cleanup, and
	// it's exactly what a process still attached to a dying terminal/session can receive.
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

// detachToNewSession re-execs this same binary as a new child in a fresh
// session (no controlling terminal), stdio redirected to /dev/null, and
// returns nil so the caller knows to let that child take over. The child
// keeps this process's argv[0]: os.Executable resolves the vpnd
// symlink we were started through to deskconn itself, and without the
// name the child would come up as the regular CLI.
//
// Can't just call setsid(2) on ourselves: it fails with EPERM if we're
// already a process group leader, which sudo's "use_pty" (Ubuntu's
// default) makes us by the time main starts. A freshly created child is
// never a group leader yet, so exec.Cmd with Setsid always works instead.
func detachToNewSession(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	cmd := exec.Command(exe, args...) //nolint:gosec
	cmd.Args[0] = os.Args[0]
	cmd.Env = append(os.Environ(), reexecEnvVar+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("re-exec detached: %w", err)
	}
	return nil
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
