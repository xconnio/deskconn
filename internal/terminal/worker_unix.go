//go:build linux || darwin

package terminal

import (
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// The Unix PTY's reading ends on its own once the shell exits.
const (
	ptyNeedsCloseOnExit = false
	ptyDrainDelay       = time.Duration(0)
)

// processTracker reaches a session's processes. go-pty starts the shell as the leader of a
// session and process group of its own, which the shell's jobs share or are part of.
type processTracker struct{ pid int }

func trackProcesses(pid int) processTracker { return processTracker{pid: pid} }

// hangup sends the shell's process group a hangup, as closing its terminal would: an
// interactive shell passes it on to its jobs.
func (p processTracker) hangup() { signalGroup(p.pid, syscall.SIGHUP) }

// killGroup kills what's left of the shell's process group.
func (p processTracker) killGroup() { signalGroup(p.pid, syscall.SIGKILL) }

// kill kills the shell's process group and every other process of its session, such as an
// interactive shell's jobs, which get process groups of their own.
func (p processTracker) kill() {
	signalGroup(p.pid, syscall.SIGKILL)
	killSession(p.pid)
}

func (p processTracker) release() {}

func signalGroup(pid int, sig syscall.Signal) {
	if pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}

// handleSignals keeps the worker running through hangups and interrupts meant for whatever
// started it, and terminates the session on SIGTERM, as the system stopping it.
func handleSignals(terminate func()) (stop func()) {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGPIPE)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			terminate()
		case <-done:
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// checkOwner refuses a file another user owns.
func checkOwner(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("owned by another user")
	}
	return nil
}

// signaledExitCode is 128+N for a process killed by signal N, as shells report it.
func signaledExitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return -1
}
