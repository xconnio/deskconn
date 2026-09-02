//go:build unix

package deskconnd_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
)

// TestCleanupShellIDKillsProcessGroup guards against a real bug: closing
// ptmx alone doesn't reliably kill a child that produces no PTY output
// (nothing ever reads it) -- the expected SIGHUP-on-hangup can race with
// startOutputReader's own concurrent blocked Read on the same file and
// silently never arrive, leaving the process running forever. cleanupShellID
// must kill the process group explicitly rather than relying on that.
//
// syscall.Kill(pid, 0) (a "does this process/group exist" probe, no signal
// actually sent) has no Windows equivalent, so this is Unix-only; Windows has
// no process-group concept for killShellProcessGroup to test the same way
// (see killShellProcessGroup's doc comment in shell_windows.go).
func TestCleanupShellIDKillsProcessGroup(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	transport := &deskconnd.FakeShellTransport{}

	shellID, _, _, startReader, err := p.BeginShellSession(common.ShellControlMsg{
		Op: common.ShellOpSize, Cols: 80, Rows: 24, Command: "sleep", Args: []string{"30"},
	}, transport)
	require.NoError(t, err)
	startReader()

	pid := p.PID(shellID)
	require.Positive(t, pid)
	require.NoError(t, syscall.Kill(pid, 0), "sanity check: process should be running before cleanup")

	p.CleanupShellID(shellID)

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 3*time.Second, 20*time.Millisecond,
		"sleep should actually be killed (and reaped) by cleanupShellID, not just have its ptmx closed")
}
