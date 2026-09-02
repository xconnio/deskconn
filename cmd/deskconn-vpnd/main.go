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

	"github.com/xconnio/deskconn/iptun"
)

// reexecEnvVar marks a process as already having gone through detachToNewSession, so it
// doesn't re-exec itself again -- see that function.
const reexecEnvVar = "DESKCONN_VPND_DETACHED"

func main() {
	app := kingpin.New("deskconn-vpnd", "Privileged backend for deskconn vpn connect")
	socketPath := app.Flag("socket", "Unix socket to listen on for the one client this process serves").
		Required().String()
	idleTimeout := app.Flag("idle-timeout", "Exit if no client connects within this long").
		Default("30s").Duration()
	kingpin.MustParse(app.Parse(os.Args[1:]))

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "deskconn-vpnd: must run as root (invoke via sudo)")
		os.Exit(1)
	}

	if os.Getenv(reexecEnvVar) == "" {
		if err := detachToNewSession(); err != nil {
			fmt.Fprintf(os.Stderr, "deskconn-vpnd: could not detach from terminal, continuing anyway: %v\n", err)
		} else {
			return // the re-exec'd child takes over from here
		}
	}

	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unixpacket", *socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deskconn-vpnd: listen on %s: %v\n", *socketPath, err)
		os.Exit(1)
	}
	// Clean up the whole temp dir LaunchHelper made for this socket, not just the socket file
	// -- "deskconn vpn start" hands off and never reaps us itself, so this is the only cleanup
	// that dir gets.
	defer func() { _ = os.RemoveAll(filepath.Dir(*socketPath)) }()

	// Only the operator's uid (or root) can reach that dir at all; this just ensures that uid
	// can connect to the socket itself, whatever umask sudo left it with.
	if err := os.Chmod(*socketPath, 0o666); err != nil { //nolint:gosec
		fmt.Fprintf(os.Stderr, "deskconn-vpnd: chmod socket: %v\n", err)
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

	server := &iptun.Server{}
	server.Serve(conn)
}

// detachToNewSession re-execs this same binary as a new child in a fresh
// session (no controlling terminal), stdio redirected to /dev/null, and
// returns nil so the caller knows to let that child take over.
//
// Can't just call setsid(2) on ourselves: it fails with EPERM if we're
// already a process group leader, which sudo's "use_pty" (Ubuntu's
// default) makes us by the time main starts. A freshly created child is
// never a group leader yet, so exec.Cmd with Setsid always works instead.
func detachToNewSession() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	cmd := exec.Command(exe, os.Args[1:]...) //nolint:gosec
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
			fmt.Fprintln(os.Stderr, "deskconn-vpnd: unexpected connection type")
			_ = conn.Close()
			return nil
		}
		return unixConn
	case err := <-acceptErrCh:
		fmt.Fprintf(os.Stderr, "deskconn-vpnd: accept: %v\n", err)
		return nil
	case <-sigCh:
		return nil
	case <-time.After(idleTimeout):
		fmt.Fprintln(os.Stderr, "deskconn-vpnd: no client connected in time, exiting")
		return nil
	}
}
