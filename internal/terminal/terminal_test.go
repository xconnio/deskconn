//go:build linux || darwin

package terminal_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	. "github.com/xconnio/deskconn/internal/terminal"
)

const workerArg = "__terminal_worker"

// TestMain runs the test binary as a worker when the tests' Manager starts one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == workerArg {
		if err := RunWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	owner    = "alice"
	waitTime = 10 * time.Second
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	// Short: a Unix socket's path is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dt")
	require.NoError(t, err)
	m := NewManager(filepath.Join(dir, "terminals"), workerArg)
	t.Cleanup(func() {
		for _, id := range m.SessionIDs() {
			if pid, err := m.WorkerPID(id); err == nil {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
		require.Eventually(t, func() bool { return len(m.SessionIDs()) == 0 },
			waitTime, 50*time.Millisecond, "workers end on SIGTERM")
		_ = os.RemoveAll(dir)
	})
	return m
}

func shellOptions(t *testing.T) CreateOptions {
	t.Helper()
	bash, err := exec.LookPath("bash")
	require.NoError(t, err)
	return CreateOptions{Owner: owner, Command: bash, Args: []string{"--norc", "--noprofile", "-i"},
		Env:     []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TERM=dumb", "PS1=$ "},
		WorkDir: t.TempDir(), Cols: 80, Rows: 24}
}

// screen collects an attachment's events.
type screen struct {
	mu        sync.Mutex
	output    strings.Builder
	exit      *int
	displaced bool
	ended     chan struct{}
}

func watch(a *Attachment) *screen {
	s := &screen{ended: make(chan struct{})}
	go func() {
		defer close(s.ended)
		for {
			ev, err := a.Next()
			s.mu.Lock()
			if err != nil {
				s.mu.Unlock()
				return
			}
			s.output.Write(ev.Output)
			if ev.Exit != nil {
				s.exit = ev.Exit
			}
			s.displaced = s.displaced || ev.Displaced
			done := ev.Exit != nil || ev.Displaced
			s.mu.Unlock()
			if done {
				_ = a.Close()
				return
			}
		}
	}()
	return s
}

func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.String()
}

// waitFor waits for output matching pattern, returning its first submatch.
func (s *screen) waitFor(t *testing.T, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var match []string
	require.Eventually(t, func() bool {
		match = re.FindStringSubmatch(s.text())
		return match != nil
	}, waitTime, 20*time.Millisecond, "waiting for %q in %q", pattern, s.text())
	if len(match) > 1 {
		return match[1]
	}
	return match[0]
}

func attach(t *testing.T, m *Manager, id string, opts AttachOptions) (*Attachment, *screen) {
	t.Helper()
	a, err := m.Attach(context.Background(), owner, id, opts)
	require.NoError(t, err)
	return a, watch(a)
}

// shellPID asks the shell for its PID.
func shellPID(t *testing.T, a *Attachment, s *screen, marker string) int {
	t.Helper()
	require.NoError(t, a.Write([]byte("echo "+marker+"=$$\n")))
	pid, err := strconv.Atoi(s.waitFor(t, marker+`=(\d+)\r?\n`))
	require.NoError(t, err)
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestCreateAttachDetachResume(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	require.True(t, ValidID(id))

	a, s := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	s.waitFor(t, `\$ `) // the first prompt, restored on attaching
	pid := shellPID(t, a, s, "first")
	require.NoError(t, a.Write([]byte("export KEPT=yes; sleep 1000 &\n")))
	s.waitFor(t, `\[1\] \d+`)

	require.NoError(t, a.Detach())
	sessions, err := m.List(ctx, owner)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, id, sessions[0].ID)
	require.Eventually(t, func() bool {
		sessions, _ := m.List(ctx, owner)
		return len(sessions) == 1 && !sessions[0].Attached
	}, waitTime, 20*time.Millisecond, "detaching leaves the session running, unattached")
	require.True(t, alive(pid))

	a2, s2 := attach(t, m, id, AttachOptions{Cols: 100, Rows: 30})
	require.Equal(t, pid, shellPID(t, a2, s2, "second"), "the same shell")
	require.NoError(t, a2.Write([]byte("echo kept=$KEPT; jobs\n")))
	s2.waitFor(t, `kept=yes`)
	s2.waitFor(t, `Running\s+sleep 1000`)
	require.NoError(t, a2.Write([]byte("stty size\n")))
	s2.waitFor(t, `30 100`)
	require.NoError(t, a2.Close())
}

func TestDaemonRestartFindsSessions(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	a, s := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	pid := shellPID(t, a, s, "before")
	// The daemon dies without detaching: its connection to the worker just ends.
	require.NoError(t, a.Close())

	restarted := NewManager(m.Dir(), workerArg)
	restarted.Reconcile(ctx)
	sessions, err := restarted.List(ctx, owner)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, id, sessions[0].ID)

	a2, s2 := attach(t, restarted, id, AttachOptions{Cols: 80, Rows: 24})
	require.Equal(t, pid, shellPID(t, a2, s2, "after"))
	require.NoError(t, a2.Close())
}

func TestTakeover(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	first, firstScreen := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	firstScreen.waitFor(t, `\$ `)

	_, err = m.Attach(ctx, owner, id, AttachOptions{})
	require.ErrorIs(t, err, ErrAttached, "attaching elsewhere needs a takeover")

	// The same client reconnecting presents its token instead.
	again, againScreen := attach(t, m, id, AttachOptions{Token: first.Token()})
	<-firstScreen.ended
	require.True(t, firstScreen.displaced)
	pid := shellPID(t, again, againScreen, "again")

	taker, takerScreen := attach(t, m, id, AttachOptions{Takeover: true})
	<-againScreen.ended
	require.True(t, againScreen.displaced)
	// The displaced attachment's input no longer reaches the shell.
	_ = again.Write([]byte("echo stale-input\n"))
	require.Equal(t, pid, shellPID(t, taker, takerScreen, "taker"))
	require.NotContains(t, takerScreen.text(), "stale-input")
	require.NoError(t, taker.Close())
}

func TestShellExitEndsSession(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	a, s := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	s.waitFor(t, `\$ `)
	require.NoError(t, a.Write([]byte("exit 3\n")))
	<-s.ended
	require.NotNil(t, s.exit, "the attachment gets the exit status")
	require.Equal(t, 3, *s.exit)

	require.Eventually(t, func() bool {
		sessions, err := m.List(ctx, owner)
		return err == nil && len(sessions) == 0
	}, waitTime, 20*time.Millisecond, "an ended session isn't kept")
	_, err = m.Attach(ctx, owner, id, AttachOptions{})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = os.Stat(m.RecordPath(id))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDetachedShellExitEndsSession(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	opts := shellOptions(t)
	opts.Args = []string{"-c", "sleep 0.5; exit 0"}
	id, err := m.Create(ctx, opts)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(m.RecordPath(id))
		return os.IsNotExist(err)
	}, waitTime, 50*time.Millisecond, "a shell exiting with nothing attached ends the session")
}

func TestTerminateKillsSessionProcesses(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	a, s := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	pid := shellPID(t, a, s, "shell")
	// A job in a process group of its own, which ignores the hangup.
	require.NoError(t, a.Write([]byte("trap '' HUP; (trap '' HUP; exec sleep 1000) & echo job=$!\n")))
	job, err := strconv.Atoi(s.waitFor(t, `job=(\d+)\r?\n`))
	require.NoError(t, err)
	require.NoError(t, a.Detach())

	require.ErrorIs(t, m.Terminate(ctx, "mallory", id), ErrNotFound, "only the owner can terminate it")
	require.NoError(t, m.Terminate(ctx, owner, id))
	// On macOS, terminating reaches the shell's process group only: no /proc to find the job.
	killsJob := runtime.GOOS == "linux"
	if !killsJob {
		t.Cleanup(func() { _ = syscall.Kill(job, syscall.SIGKILL) })
	}
	require.Eventually(t, func() bool { return !alive(pid) && (!killsJob || !alive(job)) }, waitTime,
		20*time.Millisecond, "terminating kills the shell and its jobs")
	require.Eventually(t, func() bool {
		_, err := os.Stat(m.SocketPath(id))
		return os.IsNotExist(err)
	}, waitTime, 20*time.Millisecond, "and the worker ends")
	_, err = m.Attach(ctx, owner, id, AttachOptions{})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTerminateWhileAttached(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	a, s := attach(t, m, id, AttachOptions{Cols: 80, Rows: 24})
	shellPID(t, a, s, "shell")
	require.NoError(t, m.Terminate(ctx, owner, id))
	<-s.ended
	require.NotNil(t, s.exit, "the attachment learns the session ended")
}

func TestOtherOwnersSessionsAreHidden(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)

	sessions, err := m.List(ctx, "mallory")
	require.NoError(t, err)
	require.Empty(t, sessions)
	_, err = m.Attach(ctx, "mallory", id, AttachOptions{Takeover: true})
	require.ErrorIs(t, err, ErrNotFound, "a session ID alone grants nothing")
	_, err = m.Attach(ctx, owner, "../../etc/passwd", AttachOptions{})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestWrongSecretIsRefused(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	require.ErrorIs(t, m.AttachWithSecret(ctx, id, "wrong"), ErrUnavailable)
}

func TestDeadWorkerIsUnavailable(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	id, err := m.Create(ctx, shellOptions(t))
	require.NoError(t, err)
	pid, err := m.WorkerPID(id)
	require.NoError(t, err)
	// A worker crashing leaves its record behind.
	require.NoError(t, syscall.Kill(pid, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !alive(pid) || isZombie(pid) }, waitTime, 20*time.Millisecond)

	m.Reconcile(ctx)
	_, err = os.Stat(m.RecordPath(id))
	require.ErrorIs(t, err, os.ErrNotExist, "the stale record is removed")
	_, err = m.Attach(ctx, owner, id, AttachOptions{})
	require.ErrorIs(t, err, ErrNotFound, "and no replacement shell is made")
	sessions, err := m.List(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestDetachedOutputDoesNotBlock(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "done")
	opts := shellOptions(t)
	// 64 MiB of output with nothing attached, then a marker.
	opts.Args = []string{"-c", fmt.Sprintf("head -c 67108864 /dev/zero | od -v; touch %s; sleep 1000", marker)}
	_, err := m.Create(ctx, opts)
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		60*time.Second, 100*time.Millisecond, "the command runs to the end unwatched")
}

func TestUnstartableCommand(t *testing.T) {
	m := newTestManager(t)
	opts := shellOptions(t)
	opts.Command = "/nonexistent/shell"
	_, err := m.Create(context.Background(), opts)
	require.Error(t, err)
	require.Empty(t, m.SessionIDs(), "nothing is left behind")
}

func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(data), ')')
	return end > 0 && strings.HasPrefix(string(data[end+1:]), " Z")
}
