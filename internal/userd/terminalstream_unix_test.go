//go:build linux || darwin

package userd_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/terminal"
	"github.com/xconnio/deskconn/internal/userd"
)

// TestMain runs the test binary as a terminal worker when a daemon under test starts one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == userd.TerminalWorkerCommand {
		if err := terminal.RunWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	terminalOwner = "owner-authid"
	terminalWait  = 15 * time.Second
)

// terminalDaemon is a daemon under test, with a short configuration directory: Unix socket
// paths are limited to about 100 bytes.
func terminalDaemon(t *testing.T) (*userd.Deskconn, string) {
	t.Helper()
	cfg, err := os.MkdirTemp("", "dc")
	require.NoError(t, err)
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() {
		dir := filepath.Join(cfg, "terminals")
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				data, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
				if match := regexp.MustCompile(`"pid":(\d+)`).FindSubmatch(data); match != nil {
					pid, _ := strconv.Atoi(string(match[1]))
					_ = syscall.Kill(pid, syscall.SIGTERM)
				}
			}
		}
		require.Eventually(t, func() bool {
			entries, _ := os.ReadDir(dir)
			return len(entries) == 0
		}, terminalWait, 50*time.Millisecond, "every worker ends")
		_ = os.RemoveAll(cfg)
	})
	return userd.NewDeskconn(false, cfg), cfg
}

// openTerminal makes req of d as caller over an in-memory stream.
func openTerminal(t *testing.T, d *userd.Deskconn, caller string, req common.TerminalControlMsg) (
	*deskconn.TerminalStream, common.TerminalControlMsg, error) {
	t.Helper()
	client, server := net.Pipe()
	go d.ServeTerminalStream(server, caller)
	ctx, cancel := context.WithTimeout(context.Background(), terminalWait)
	defer cancel()
	return deskconn.StartTerminal(ctx, client, func() { _ = client.Close() }, req)
}

// terminalScreen collects an attached stream's output and end.
type terminalScreen struct {
	mu        sync.Mutex
	output    strings.Builder
	exit      *int
	displaced bool
	ended     chan struct{}
}

func watchTerminal(ts *deskconn.TerminalStream) *terminalScreen {
	s := &terminalScreen{ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		for {
			kind, msg, output, err := ts.Recv()
			if err != nil {
				return
			}
			s.mu.Lock()
			if kind == common.ShellMsgData {
				s.output.Write(output)
			}
			switch msg.Op {
			case common.TerminalOpExit:
				s.exit = msg.ExitCode
			case common.TerminalOpDisplaced:
				s.displaced = true
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *terminalScreen) waitFor(t *testing.T, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var match []string
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		match = re.FindStringSubmatch(s.output.String())
		return match != nil
	}, terminalWait, 20*time.Millisecond, "waiting for %q", pattern)
	return match[len(match)-1]
}

func askPID(t *testing.T, ts *deskconn.TerminalStream, s *terminalScreen, marker string) int {
	t.Helper()
	require.NoError(t, ts.SendInput([]byte("echo "+marker+"=$$\n")))
	pid, err := strconv.Atoi(s.waitFor(t, marker+`=(\d+)\r?\n`))
	require.NoError(t, err)
	return pid
}

func listTerminals(t *testing.T, d *userd.Deskconn, caller string) []common.TerminalSession {
	t.Helper()
	ts, reply, err := openTerminal(t, d, caller, common.TerminalControlMsg{Op: common.TerminalOpList})
	require.NoError(t, err)
	ts.Close()
	return reply.Sessions
}

func TestTerminalSessionSurvivesDisconnect(t *testing.T) {
	d, _ := terminalDaemon(t)
	ts, reply, err := openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpCreate, Cols: 80, Rows: 24})
	require.NoError(t, err)
	require.NotEmpty(t, reply.Session)
	require.NotEmpty(t, reply.Token)
	s := watchTerminal(ts)
	pid := askPID(t, ts, s, "created")

	// The client goes away without a word.
	ts.Close()
	require.Eventually(t, func() bool {
		sessions := listTerminals(t, d, terminalOwner)
		return len(sessions) == 1 && sessions[0].ID == reply.Session && !sessions[0].Attached
	}, terminalWait, 50*time.Millisecond)

	ts, again, err := openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: reply.Session, Cols: 80, Rows: 24})
	require.NoError(t, err)
	require.Equal(t, reply.Session, again.Session)
	s = watchTerminal(ts)
	require.Equal(t, pid, askPID(t, ts, s, "resumed"), "the same shell")

	// Detaching explicitly leaves it running too.
	require.NoError(t, ts.SendControl(common.TerminalControlMsg{Op: common.TerminalOpDetach}))
	<-s.ended
	require.Nil(t, s.exit)
	require.Len(t, listTerminals(t, d, terminalOwner), 1)

	ts, _, err = openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpTerminate, Session: reply.Session})
	require.NoError(t, err)
	ts.Close()
	require.Empty(t, listTerminals(t, d, terminalOwner))
}

func TestTerminalSessionSurvivesDaemonRestart(t *testing.T) {
	d, cfg := terminalDaemon(t)
	ts, reply, err := openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpCreate, Cols: 80, Rows: 24})
	require.NoError(t, err)
	s := watchTerminal(ts)
	pid := askPID(t, ts, s, "before")
	require.NoError(t, ts.SendInput([]byte("sleep 2; echo long-command-finished\n")))
	ts.Close()

	restarted := userd.NewDeskconn(false, cfg)
	sessions := listTerminals(t, restarted, terminalOwner)
	require.Len(t, sessions, 1)
	require.Equal(t, reply.Session, sessions[0].ID)

	ts, _, err = openTerminal(t, restarted, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: reply.Session, Cols: 80, Rows: 24})
	require.NoError(t, err)
	s = watchTerminal(ts)
	s.waitFor(t, "long-command-finished")
	require.Equal(t, pid, askPID(t, ts, s, "after"))
	ts.Close()
}

func TestTerminalSessionExit(t *testing.T) {
	d, _ := terminalDaemon(t)
	ts, reply, err := openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpCreate, Cols: 80, Rows: 24})
	require.NoError(t, err)
	s := watchTerminal(ts)
	askPID(t, ts, s, "shell")
	require.NoError(t, ts.SendInput([]byte("exit 5\n")))
	<-s.ended
	require.NotNil(t, s.exit)
	require.Equal(t, 5, *s.exit)
	require.Empty(t, listTerminals(t, d, terminalOwner))

	_, _, err = openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: reply.Session})
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrNotFound), "%v", err)
	require.Empty(t, listTerminals(t, d, terminalOwner), "attaching never creates a session")
}

func TestTerminalSessionOwnership(t *testing.T) {
	d, _ := terminalDaemon(t)
	ts, reply, err := openTerminal(t, d, terminalOwner,
		common.TerminalControlMsg{Op: common.TerminalOpCreate, Cols: 80, Rows: 24})
	require.NoError(t, err)
	s := watchTerminal(ts)
	pid := askPID(t, ts, s, "owner")

	const other = "other-authid"
	require.Empty(t, listTerminals(t, d, other))
	_, _, err = openTerminal(t, d, other, common.TerminalControlMsg{Op: common.TerminalOpAttach,
		Session: reply.Session, Takeover: true})
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrNotFound), "%v", err)
	_, _, err = openTerminal(t, d, other, common.TerminalControlMsg{Op: common.TerminalOpTerminate,
		Session: reply.Session})
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrNotFound), "%v", err)

	// The owner, from another client, needs a takeover.
	_, _, err = openTerminal(t, d, terminalOwner, common.TerminalControlMsg{Op: common.TerminalOpAttach,
		Session: reply.Session})
	require.True(t, deskconn.IsTerminalError(err, common.TerminalErrAttached), "%v", err)
	taker, _, err := openTerminal(t, d, terminalOwner, common.TerminalControlMsg{Op: common.TerminalOpAttach,
		Session: reply.Session, Takeover: true, Cols: 80, Rows: 24})
	require.NoError(t, err)
	<-s.ended
	require.True(t, s.displaced, "the displaced client is told")
	takerScreen := watchTerminal(taker)
	require.Equal(t, pid, askPID(t, taker, takerScreen, "taker"))
	taker.Close()
}
