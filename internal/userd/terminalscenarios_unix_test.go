//go:build linux || darwin

package userd_test

// End-to-end scenarios of the terminal restoration guarantee (see docs/terminal-sessions.md,
// Validation): desk shell's attachment, a daemon and real terminal workers and shells.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
)

func createTerminal() common.TerminalControlMsg {
	return common.TerminalControlMsg{Op: common.TerminalOpCreate, Cols: 80, Rows: 24}
}

// dropConnections closes the machine's end of every connection c has made.
func (c *cliTerminal) dropConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, server := range c.servers {
		_ = server.Close()
	}
}

func (c *cliTerminal) setOffline(offline bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offline = offline
}

// waitReattached waits for c to reconnect after its connection dropped: its screen is
// redrawn once it has.
func (c *cliTerminal) waitReattached(t *testing.T) {
	t.Helper()
	c.waitFor(t, `connection lost; reconnecting[^\x00]*\x1b\[2J`)
}

// workerPID is the PID of session id's terminal worker, from its record.
func workerPID(t *testing.T, cfg, id string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg, "terminals", id+".json"))
	require.NoError(t, err)
	match := regexp.MustCompile(`"pid":(\d+)`).FindSubmatch(data)
	require.NotNil(t, match)
	pid, err := strconv.Atoi(string(match[1]))
	require.NoError(t, err)
	return pid
}

// A command writing output on through a dropped connection: the terminal ends up with all
// of it, and repeats only what the restored screen redraws.
func TestScenarioOutputAcrossReconnect(t *testing.T) {
	d, _ := terminalDaemon(t)
	c := runCLITerminal(t, d, createTerminal())
	pid := c.pid(t, "before")
	const lines = 300
	c.typeKeys(t, "for i in $(seq 1 "+strconv.Itoa(lines)+"); do echo line-$i; sleep 0.01; done; echo loop-$((1+1))\n")
	c.waitFor(t, `line-50\r\n`)
	c.dropConnections()
	c.waitReattached(t)
	c.waitFor(t, `loop-2\r\n`)
	require.Equal(t, pid, c.pid(t, "after"), "the same shell")

	counts := map[int]int{}
	for _, match := range regexp.MustCompile(`line-(\d+)\r\n`).FindAllStringSubmatch(c.output.String(), -1) {
		n, _ := strconv.Atoi(match[1])
		counts[n]++
	}
	twice := 0
	for n := 1; n <= lines; n++ {
		require.Positive(t, counts[n], "line %d is missing", n)
		require.LessOrEqual(t, counts[n], 2, "line %d", n)
		if counts[n] == 2 {
			twice++
		}
	}
	// At most a screenful, what the terminal showed as the connection dropped, comes again.
	require.LessOrEqual(t, twice, 24)
}

// Keys typed while reconnecting don't reach the shell, then or later: a command is never
// run without the user seeing it run.
func TestScenarioInputWhileReconnectingIsDropped(t *testing.T) {
	d, _ := terminalDaemon(t)
	c := runCLITerminal(t, d, createTerminal())
	pid := c.pid(t, "before")
	c.setOffline(true)
	c.dropConnections()
	c.waitFor(t, `connection lost; reconnecting`)
	c.typeKeys(t, "echo typed-$((6*7))\n")
	c.setOffline(false)
	c.waitReattached(t)
	require.Equal(t, pid, c.pid(t, "after"))
	require.NotContains(t, c.output.String(), "typed-42")
}

// The daemon restarts while a command runs: the client attaches again, through the new
// daemon, to the same shell, and the command's output goes on.
func TestScenarioDaemonRestartWhileCommandRuns(t *testing.T) {
	d, cfg := terminalDaemon(t)
	c := runCLITerminal(t, d, createTerminal())
	pid := c.pid(t, "before")
	c.typeKeys(t, "echo started-$((1+1)); sleep 2; echo finished-$((2+3))\n")
	c.waitFor(t, `started-2\r\n`)
	c.mu.Lock()
	c.daemon = userd.NewDeskconn(false, cfg)
	c.mu.Unlock()
	c.dropConnections()
	c.waitReattached(t)
	c.waitFor(t, `finished-5\r\n`)
	require.Equal(t, pid, c.pid(t, "after"))
}

// More output than a session's default history, written while detached, doesn't hold
// the command up, the history stays within its budget, and attaching tells what's gone.
func TestScenarioOutputPastHistoryLimitWhileDetached(t *testing.T) {
	d, cfg := terminalDaemon(t)
	ts, reply, err := openTerminal(t, d, terminalOwner, createTerminal())
	require.NoError(t, err)
	s := watchTerminal(ts)
	pid := askPID(t, ts, s, "before")
	marker := filepath.Join(cfg, "written")
	// 20 MiB of 80-byte lines.
	require.NoError(t, ts.SendInput([]byte("sleep 1; head -c 20971520 /dev/zero | tr '\\0' x | fold -w 79; touch "+
		marker+"\n")))
	ts.Close()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		60*time.Second, 100*time.Millisecond, "the command runs to the end unwatched")

	since := reply.Offset
	ts, again, err := openTerminal(t, d, terminalOwner, common.TerminalControlMsg{Op: common.TerminalOpAttach,
		Session: reply.Session, Cols: 80, Rows: 24, Since: &since})
	require.NoError(t, err)
	require.True(t, again.HistoryGap, "the history from the start is gone")
	require.Greater(t, again.Offset, uint64(20<<20))
	require.Greater(t, again.Snapshot, 8<<20, "the history kept")
	require.LessOrEqual(t, again.Snapshot, 17<<20, "within the 16 MiB budget, and a screen")
	s = watchTerminal(ts)
	require.Equal(t, pid, askPID(t, ts, s, "after"))
	ts.Close()
}

// Ending sessions gives the history they kept back to new ones.
func TestScenarioEndedSessionsFreeTheirHistory(t *testing.T) {
	d, _ := terminalDaemon(t)
	d.SetTerminalHistoryLimits(1<<20, 1<<20)
	ts, first, err := openTerminal(t, d, terminalOwner, createTerminal())
	require.NoError(t, err)
	ts.Close()
	_, _, err = openTerminal(t, d, terminalOwner, createTerminal())
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrLimit), "%v", err)

	ts, _, err = openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpTerminate, Session: first.Session})
	require.NoError(t, err)
	ts.Close()
	ts, _, err = openTerminal(t, d, terminalOwner, createTerminal())
	require.NoError(t, err, "terminating freed it")
	s := watchTerminal(ts)
	askPID(t, ts, s, "second")
	require.NoError(t, ts.SendInput([]byte("exit\n")))
	<-s.ended

	ts, _, err = openTerminal(t, d, terminalOwner, createTerminal())
	require.NoError(t, err, "exiting freed it")
	ts.Close()
}

// Workers failing, as in a crash or a reboot, end their sessions: an attached client is
// told, and attaching again fails rather than starting another shell.
func TestScenarioWorkerFailureEndsTheSession(t *testing.T) {
	d, cfg := terminalDaemon(t)
	ts, detached, err := openTerminal(t, d, terminalOwner, createTerminal())
	require.NoError(t, err)
	ts.Close()
	c := runCLITerminal(t, d, createTerminal())
	c.pid(t, "shell")
	sessions := listTerminals(t, d, terminalOwner)
	require.Len(t, sessions, 2)
	for _, session := range sessions {
		require.NoError(t, syscall.Kill(workerPID(t, cfg, session.ID), syscall.SIGKILL))
	}
	c.wait(t)
	require.ErrorContains(t, c.err, "has ended")

	// The detached session's record is left behind, as after a reboot: a restarted daemon
	// finds no session, and attaching creates none.
	restarted := userd.NewDeskconn(false, cfg)
	require.Empty(t, listTerminals(t, restarted, terminalOwner))
	_, _, err = openTerminal(t, restarted, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: detached.Session})
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrNotFound), "%v", err)
	require.Empty(t, listTerminals(t, restarted, terminalOwner))
	entries, err := os.ReadDir(filepath.Join(cfg, "terminals"))
	require.NoError(t, err)
	require.Empty(t, entries, "their records are removed")
}
