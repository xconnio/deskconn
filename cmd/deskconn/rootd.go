//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xconnio/deskconn/internal/rootd"
)

// runRootd is the root helper, for now the privileged backend for "desk vpn": it serves
// exactly one VPN helper client on a unix socket, then exits.
func runRootd(flags rootdFlags) {
	socketPath, idleTimeout := flags.socketPath, flags.idleTimeout

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "deskconn-rootd: must run as root (invoke via sudo)")
		os.Exit(1)
	}
	if *flags.detach {
		if err := detachRootd(*flags.pidFile); err != nil {
			fmt.Fprintf(os.Stderr, "deskconn-rootd: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// fail exits on an error, which skips deferred cleanup, so it drops the pid file itself.
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "deskconn-rootd: "+format+"\n", args...)
		if *flags.pidFile != "" {
			_ = os.Remove(*flags.pidFile)
		}
		os.Exit(1)
	}
	if *flags.pidFile != "" {
		defer func() { _ = os.Remove(*flags.pidFile) }()
	}

	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unixpacket", *socketPath)
	if err != nil {
		fail("listen on %s: %v", *socketPath, err)
	}
	// Clean up the whole temp dir LaunchVPNHelper made for this socket, not just the socket
	// file -- "desk vpn serve" hands off and returns, so this is the only cleanup that dir gets.
	defer func() { _ = os.RemoveAll(filepath.Dir(*socketPath)) }()

	// Only the operator's uid (or root) can reach that dir at all; this just ensures that uid
	// can connect to the socket itself, whatever umask it was started with.
	if err := os.Chmod(*socketPath, 0o666); err != nil { //nolint:gosec
		fail("chmod socket: %v", err)
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

	server := &rootd.Server{}
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
			fmt.Fprintln(os.Stderr, "deskconn-rootd: unexpected connection type")
			_ = conn.Close()
			return nil
		}
		return unixConn
	case err := <-acceptErrCh:
		fmt.Fprintf(os.Stderr, "deskconn-rootd: accept: %v\n", err)
		return nil
	case <-sigCh:
		return nil
	case <-time.After(idleTimeout):
		fmt.Fprintln(os.Stderr, "deskconn-rootd: no client connected in time, exiting")
		return nil
	}
}

// detachRootd starts this same command again minus --detach, as its own session with its
// output going to a log next to pidFile, records its pid there, and returns -- for where
// there's no systemd to run the root helper as a unit: sudo returns once this does, and the
// copy outlives the terminal it was started from. The copy removes pidFile when it exits.
func detachRootd(pidFile string) error {
	if pidFile == "" {
		return fmt.Errorf("--detach needs --pid-file")
	}
	if err := os.MkdirAll(filepath.Dir(pidFile), 0o755); err != nil { //nolint:gosec
		return err
	}
	logPath := strings.TrimSuffix(pidFile, ".pid") + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := slices.DeleteFunc(slices.Clone(os.Args[1:]), func(arg string) bool { return arg == "--detach" })
	cmd := exec.Command(exe, args...) //nolint:gosec
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Written here rather than by the copy so it's in place by the time sudo returns, for
	// the launcher to tell the helper is running while its socket comes up.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil { //nolint:gosec
		_ = cmd.Process.Kill()
		return err
	}
	return cmd.Process.Release()
}
