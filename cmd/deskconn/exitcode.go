package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/xconnio/deskconn/internal/deskconn"
)

// Exit statuses. A command run on a machine (desk shell, file ls, agent resume --remote)
// exits with that command's own status instead; like ssh, a connection failure is 255
// so it can be told apart from most of those.
const (
	exitFailure    = 1
	exitUsage      = 2
	exitConnection = 255
)

// errAlreadyReported is a failure whose reason has already been shown on stderr.
var errAlreadyReported = errors.New("already reported")

// usageError is an invalid command line that kingpin can't catch itself.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// exitStatus is the process exit status for a command that failed with err, and
// whether err still needs printing: a remote command's or child process's own
// failure has already been reported on stderr by that command.
func exitStatus(err error) (status int, report bool) {
	if errors.Is(err, errAlreadyReported) {
		return exitFailure, false
	}
	var remote *deskconn.ExitError
	if errors.As(err, &remote) {
		return remoteExitStatus(remote.Code), false
	}
	var child *exec.ExitError
	if errors.As(err, &child) && child.ExitCode() > 0 {
		return child.ExitCode(), false
	}
	var conn *deskconn.ConnectionError
	if errors.As(err, &conn) {
		return exitConnection, true
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return exitUsage, true
	}
	return exitFailure, true
}

// remoteExitStatus is code as this process can exit with it. Outside Windows only its
// low 8 bits survive, so a code that doesn't fit (a Windows machine's 0xC0000005, or
// 256 which would read as success) becomes a plain failure.
func remoteExitStatus(code int) int {
	if runtime.GOOS != goosWindows && (code < 1 || code > 255) {
		return exitFailure
	}
	return code
}

// exitOnErr ends the process with err's exit status, reporting err on stderr, if
// err isn't nil.
func exitOnErr(err error) {
	if err == nil {
		return
	}
	status, report := exitStatus(err)
	if report {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(status)
}
