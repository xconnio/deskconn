//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/vpnwin"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	vpnConnectLogName = "vpn-connect.log"

	// vpnDisconnectTimeout bounds how long "vpn disconnect" waits for the tunnel's teardown.
	vpnDisconnectTimeout = 45 * time.Second
	vpnPollInterval      = 200 * time.Millisecond

	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoConsole      = 0x00008000
	swHide                = 0
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       windows.Handle
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    windows.Handle
	idList     uintptr
	class      *uint16
	keyClass   windows.Handle
	hotKey     uint32
	icon       windows.Handle
	process    windows.Handle
}

func runRootd(_ rootdFlags) {
	fmt.Fprintln(os.Stderr, "desk: the root helper is not supported on this platform")
}

func callVPNProcedure(ctx context.Context, cfgDirectory, procedure string) error {
	session, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()
	return session.Call(procedure).DoContext(ctx).Err
}

// On Windows the exit node runs inside userd and needs no root helper: see userd/iptunnel_windows.go.
func runVPNServe(ctx context.Context, cfgDirectory string) {
	if err := callVPNProcedure(ctx, cfgDirectory, common.ProcedureProxyVPNStart); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("Serving in the background: this machine can now be used as a VPN exit node, " +
		"one connection at a time.")
	fmt.Println("Run \"desk vpn stop\" (from anywhere) to stop.")
}

func runVPNStop(ctx context.Context, cfgDirectory string) {
	if err := callVPNProcedure(ctx, cfgDirectory, common.ProcedureProxyVPNStop); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("Stopped serving.")
}

// vpnEventNames names this user's two events. The launch event exists only while "vpn connect" is
// starting a tunnel process, tells that process it's the tunnel, and gets set once the tunnel is
// up. The stop event exists for as long as a tunnel process runs, which says one is up, and setting
// it stops that process.
func vpnEventNames() (launch, stop, sid string, err error) {
	current, err := user.Current()
	if err != nil {
		return "", "", "", err
	}
	return `Local\deskconn-vpn-launch-` + current.Uid, `Local\deskconn-vpn-stop-` + current.Uid, current.Uid, nil
}

func openEvent(name string, access uint32) (windows.Handle, error) {
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	return windows.OpenEvent(access, false, ptr)
}

// eventExists says whether name is open anywhere. Being refused access still says it is.
func eventExists(name string) bool {
	event, err := openEvent(name, windows.SYNCHRONIZE)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	_ = windows.CloseHandle(event)
	return true
}

func setEvent(name string) error {
	event, err := openEvent(name, windows.EVENT_MODIFY_STATE)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(event) }()
	return windows.SetEvent(event)
}

// runVPNConnect routes this machine's internet traffic through machine and returns once the tunnel
// is up, leaving it running in the background until "vpn disconnect". Wintun needs elevation, so
// this starts itself again through UAC and that process, which has no console, runs the tunnel.
func runVPNConnect(ctx context.Context, cfgDirectory, realm, machine string, routes common.VPNRoutes) {
	if !routes.Empty() {
		fmt.Fprintln(os.Stderr, "vpn connect --route is not supported on Windows yet: leave it out to tunnel all traffic")
		return
	}
	launch, stop, _, err := vpnEventNames()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if eventExists(launch) {
		if !windows.GetCurrentProcessToken().IsElevated() {
			fmt.Fprintln(os.Stderr, "another \"vpn connect\" is already starting")
			return
		}
		runVPNTunnel(ctx, cfgDirectory, realm, machine)
		return
	}

	if eventExists(stop) {
		fmt.Fprintln(os.Stderr, "already connected; run \"deskconn vpn disconnect\" first")
		return
	}
	if err := vpnwin.CheckDLL(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	launchPtr, err := windows.UTF16PtrFromString(launch)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	ready, err := windows.CreateEvent(nil, 1, 0, launchPtr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = windows.CloseHandle(ready) }()

	logPath := filepath.Join(cfgDirectory, vpnConnectLogName)
	_ = os.Remove(logPath)

	fmt.Println("Approve the Windows prompt to grant the tunnel the network access it needs.")
	process, err := startElevated(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = windows.CloseHandle(process) }()
	fmt.Printf("Connecting to %q...\n", machine)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	handles := []windows.Handle{ready, process}
	for {
		event, err := windows.WaitForMultipleObjects(handles, false, uint32(vpnPollInterval.Milliseconds()))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		switch event {
		case windows.WAIT_OBJECT_0:
			fmt.Printf("Tunnel up: routing this machine's internet traffic through %q.\n", machine)
			fmt.Println("Run \"deskconn vpn disconnect\" to stop.")
			return
		case windows.WAIT_OBJECT_0 + 1:
			reportTunnelFailure(logPath)
			return
		}

		select {
		case <-sigCh:
			// The tunnel process has its own console, so Ctrl-C doesn't reach it. It may not have
			// got as far as listening for this yet, so keep at it until it's gone.
			sigCh <- os.Interrupt
			_ = setEvent(stop)
		default:
		}
	}
}

// reportTunnelFailure shows what the tunnel process logged, which is all that's left of it.
func reportTunnelFailure(logPath string) {
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		fmt.Fprintln(os.Stderr, "the tunnel process exited before it could report anything. The account "+
			"approving the Windows prompt must be the one logged in to deskconn")
		return
	}
	_, _ = os.Stderr.Write(data)
}

// startElevated runs this executable with args through UAC, without a window.
func startElevated(args []string) (windows.Handle, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	var params string
	for _, arg := range args {
		params += " " + syscall.EscapeArg(arg)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	parameters, err := windows.UTF16PtrFromString(params)
	if err != nil {
		return 0, err
	}

	info := shellExecuteInfo{
		mask:       seeMaskNoCloseProcess | seeMaskNoConsole,
		verb:       verb,
		file:       file,
		parameters: parameters,
		show:       swHide,
	}
	info.size = uint32(unsafe.Sizeof(info))

	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	if ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
		if errors.Is(callErr, windows.ERROR_CANCELLED) {
			return 0, errors.New("vpn connect needs administrator rights: the Windows prompt was declined")
		}
		return 0, fmt.Errorf("start the tunnel process: %w", callErr)
	}
	if info.process == 0 {
		return 0, errors.New("start the tunnel process: nothing was started")
	}
	return info.process, nil
}

// createStopEvent claims the stop event, which an unelevated "vpn disconnect" has to be able to set
// although an elevated process owns it, hence the explicit access for the user.
func createStopEvent(stop, sid string) (windows.Handle, error) {
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:(A;;GA;;;%s)(A;;GA;;;SY)(A;;GA;;;BA)", sid))
	if err != nil {
		return 0, err
	}
	attrs := windows.SecurityAttributes{SecurityDescriptor: sd}
	attrs.Length = uint32(unsafe.Sizeof(attrs))

	name, err := windows.UTF16PtrFromString(stop)
	if err != nil {
		return 0, err
	}
	event, err := windows.CreateEvent(&attrs, 1, 0, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(event)
		return 0, errors.New("already connected; run \"deskconn vpn disconnect\" first")
	}
	return event, err
}

// runVPNTunnel is the elevated process runVPNConnect starts. It logs to a file, having no console,
// and only exits, closing the stop event, once its teardown is done.
func runVPNTunnel(cliCtx context.Context, cfgDirectory, realm, machine string) {
	// Wintun's chatter, which goes here and would read as failures. Its errors are returned too.
	stdlog.SetOutput(io.Discard)

	if logFile, err := os.OpenFile(filepath.Join(cfgDirectory, vpnConnectLogName),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		os.Stdout, os.Stderr = logFile, logFile
		log.SetOutput(logFile)
		defer func() { _ = logFile.Close() }()
	}

	launch, stop, sid, err := vpnEventNames()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	launchEvent, err := openEvent(launch, windows.EVENT_MODIFY_STATE)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	launchOpen := true
	closeLaunch := func() {
		if launchOpen {
			launchOpen = false
			_ = windows.CloseHandle(launchEvent)
		}
	}
	defer closeLaunch()

	stopEvent, err := createStopEvent(stop, sid)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() { _ = windows.CloseHandle(stopEvent) }()

	ctx, cancel := context.WithCancel(cliCtx)
	defer cancel()
	go func() {
		if event, err := windows.WaitForSingleObject(stopEvent, windows.INFINITE); err == nil &&
			event == windows.WAIT_OBJECT_0 {
			cancel()
		}
	}()

	session, err := deskconn.ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer closeSessionWithTimeout(session)

	err = deskconn.ConnectVPNClient(ctx, session, func() {
		fmt.Printf("Tunnel up: routing this machine's internet traffic through %q.\n", machine)
		_ = windows.SetEvent(launchEvent)
		// Gone from now on, so the next "vpn connect" isn't taken for a tunnel process.
		closeLaunch()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Println("Tunnel down")
}

// closeSessionWithTimeout closes session with a bound, as closing is network I/O.
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

// runVPNDisconnect stops the tunnel "vpn connect" left running, and waits for its teardown.
func runVPNDisconnect(_ context.Context, cfgDirectory string) {
	_, stop, _, err := vpnEventNames()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	if !eventExists(stop) {
		fmt.Fprintln(os.Stderr, "not connected")
		return
	}
	if err := setEvent(stop); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println("Disconnecting...")

	deadline := time.Now().Add(vpnDisconnectTimeout)
	for eventExists(stop) {
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "timed out waiting for the tunnel to stop, see %s\n",
				filepath.Join(cfgDirectory, vpnConnectLogName))
			return
		}
		time.Sleep(vpnPollInterval)
	}
	fmt.Println("Disconnected.")
}
