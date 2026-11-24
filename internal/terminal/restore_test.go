//go:build linux || darwin

package terminal_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	. "github.com/xconnio/deskconn/internal/terminal"
)

// client is a terminal attached to a session: everything the attachment sends, its snapshot
// and then the session's output, goes on its screen, as on a real terminal.
type client struct {
	a      *Attachment
	mu     sync.Mutex
	screen *Screen
	// received counts the output after the snapshot.
	received int
	snapshot int
	ended    chan struct{}
}

func attachClient(t *testing.T, m *Manager, id string, cols, rows int, opts AttachOptions) *client {
	t.Helper()
	opts.Cols, opts.Rows = uint16(cols), uint16(rows)
	a, err := m.Attach(context.Background(), owner, id, opts)
	require.NoError(t, err)
	c := &client{a: a, screen: NewScreen(cols, rows, 64<<20), snapshot: a.Snapshot, ended: make(chan struct{})}
	go func() {
		defer close(c.ended)
		for {
			ev, err := a.Next()
			if err != nil || ev.Exit != nil || ev.Displaced {
				return
			}
			c.mu.Lock()
			c.screen.Write(ev.Output)
			if over := c.snapshot - len(ev.Output); over < 0 {
				c.received -= over
				c.snapshot = 0
			} else {
				c.snapshot = over
			}
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = a.Close() })
	return c
}

// detach detaches the client, and waits for the session to let go of it.
func (c *client) detach(t *testing.T) {
	t.Helper()
	require.NoError(t, c.a.Detach())
	select {
	case <-c.ended:
	case <-time.After(waitTime):
		t.Fatal("the session doesn't let go of a detached client")
	}
}

// seen is the output offset the client has seen up to.
func (c *client) seen() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.a.Offset + uint64(c.received)
}

func (c *client) look(fn func(s *Screen) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fn(c.screen)
}

func (c *client) waitFor(t *testing.T, what string, fn func(s *Screen) bool) {
	t.Helper()
	require.Eventually(t, func() bool { return c.look(fn) }, waitTime, 20*time.Millisecond,
		"waiting for %s; the screen shows:\n%s", what, c.text())
}

func (c *client) waitText(t *testing.T, pattern string) {
	t.Helper()
	re := regexp.MustCompile("(?m)" + pattern)
	c.waitFor(t, pattern, func(s *Screen) bool { return re.MatchString(s.Text()) })
}

func (c *client) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.screen.Text()
}

// quiet waits for the session to stop writing to the client for a while.
func (c *client) quiet(t *testing.T) {
	t.Helper()
	last, since := c.seen(), time.Now()
	require.Eventually(t, func() bool {
		if now := c.seen(); now != last {
			last, since = now, time.Now()
		}
		return time.Since(since) > 500*time.Millisecond
	}, waitTime, 50*time.Millisecond, "the session goes quiet")
}

// lines is all the client has: its history, then its normal screen.
func (c *client) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(c.screen.History(), strings.Split(c.screen.MainText(), "\n")...)
}

// requireSameScreen checks a client attaching shows what one that watched all along does.
func requireSameScreen(t *testing.T, watched, attached *client) {
	t.Helper()
	watched.mu.Lock()
	defer watched.mu.Unlock()
	attached.mu.Lock()
	defer attached.mu.Unlock()
	require.Equal(t, watched.screen.MainText(), attached.screen.MainText(), "normal screen")
	require.Equal(t, watched.screen.Text(), attached.screen.Text(), "active screen")
	require.Equal(t, watched.screen.History(), attached.screen.History(), "history")
	require.Equal(t, watched.screen.State(), attached.screen.State(), "cursor, modes and pens")
}

func TestReattachRestoresScreen(t *testing.T) {
	m := newTestManager(t)
	id, err := m.Create(context.Background(), shellOptions(t))
	require.NoError(t, err)

	first := attachClient(t, m, id, 80, 24, AttachOptions{})
	first.waitText(t, `\$$`)
	require.NoError(t, first.a.Write([]byte(
		"for i in $(seq 1 40); do echo line $i; done; printf '\\033[1;31mred\\033[0m\\n'\n")))
	first.waitText(t, `red\n\$$`)
	first.quiet(t)
	first.detach(t)

	second := attachClient(t, m, id, 80, 24, AttachOptions{})
	require.Positive(t, second.a.Snapshot)
	require.False(t, second.a.HistoryGap)
	second.waitText(t, `red\n\$$`)
	requireSameScreen(t, first, second)
	require.Contains(t, second.lines(), "line 1", "the history is restored too")

	// The restored shell takes input as before.
	require.NoError(t, second.a.Write([]byte("echo after\n")))
	second.waitText(t, `after\n\$$`)
}

func TestReattachAtAnotherSize(t *testing.T) {
	m := newTestManager(t)
	id, err := m.Create(context.Background(), shellOptions(t))
	require.NoError(t, err)
	first := attachClient(t, m, id, 80, 24, AttachOptions{})
	first.waitText(t, `\$$`)
	require.NoError(t, first.a.Write([]byte("echo "+strings.Repeat("x", 70)+"\n")))
	first.waitText(t, `x{70}\n\$$`)
	first.detach(t)

	// The screen is restored at the new size, then the programs in it redraw for it.
	second := attachClient(t, m, id, 40, 10, AttachOptions{})
	second.waitText(t, `\$$`)
	require.NoError(t, second.a.Write([]byte("stty size\n")))
	second.waitText(t, `10 40\n\$$`)
}

// TestSnapshotHandover runs output through attaching, reattaching and reconnecting while
// it's written: what the client ends up with has every line once.
func TestSnapshotHandover(t *testing.T) {
	m := newTestManager(t)
	id, err := m.Create(context.Background(), shellOptions(t))
	require.NoError(t, err)
	starter := attachClient(t, m, id, 80, 24, AttachOptions{})
	starter.waitText(t, `\$$`)
	const count = 200000
	require.NoError(t, starter.a.Write(
		fmt.Appendf(nil, "clear; for i in $(seq 1 %d); do echo n$i; done; echo done\n", count)))

	// A client attaching partway through.
	time.Sleep(100 * time.Millisecond)
	c := attachClient(t, m, id, 80, 24, AttachOptions{Takeover: true})
	require.Eventually(t, func() bool { return c.seen() > c.a.Offset+10000 }, waitTime, time.Millisecond,
		"output goes on after the snapshot")
	// Its connection drops; reconnecting with what it's seen, only what it missed is restored.
	require.NoError(t, c.a.Close())
	<-c.ended
	seen := c.seen()
	again := attachClient(t, m, id, 80, 24, AttachOptions{Since: &seen})
	again.mu.Lock()
	again.screen = c.screen
	again.mu.Unlock()
	again.waitText(t, `done\n\$$`)
	require.Greater(t, again.seen(), again.a.Offset+10000, "output went on after the second snapshot")

	numbers := regexp.MustCompile(`^n(\d+)$`)
	next := 1
	for _, l := range again.lines() {
		if match := numbers.FindStringSubmatch(l); match != nil {
			n, _ := strconv.Atoi(match[1])
			require.Equal(t, next, n, "every line once, in order")
			next++
		}
	}
	require.Equal(t, count+1, next)
}

func TestHistoryLimitAndGap(t *testing.T) {
	m := newTestManager(t)
	m.SetHistoryLimits(64<<10, 1<<30)
	id, err := m.Create(context.Background(), shellOptions(t))
	require.NoError(t, err)
	c := attachClient(t, m, id, 80, 24, AttachOptions{})
	require.NoError(t, c.a.Write([]byte("seq 1 100000; echo done\n")))
	c.waitText(t, `done\n\$$`)
	c.detach(t)

	again := attachClient(t, m, id, 80, 24, AttachOptions{})
	require.True(t, again.a.HistoryGap, "a client is told history it's not given is gone")
	require.Less(t, again.a.Snapshot, 64<<10+80*24*32, "the snapshot is bounded by the history kept")
	again.waitText(t, `done\n\$$`)
	require.Contains(t, again.lines(), "100000")
}

func TestHistoryTotalLimit(t *testing.T) {
	m := newTestManager(t)
	m.SetHistoryLimits(4<<20, 9<<20)
	ctx := context.Background()
	var limits []int
	for range 3 {
		id, err := m.Create(ctx, shellOptions(t))
		require.NoError(t, err)
		limit, err := m.HistoryLimit(id)
		require.NoError(t, err)
		limits = append(limits, limit)
	}
	require.Equal(t, []int{4 << 20, 4 << 20, 1 << 20}, limits, "a session gets what's left of the total")
	_, err := m.Create(ctx, shellOptions(t))
	require.ErrorIs(t, err, ErrLimit)
}

func TestVimRestoresAlternateScreen(t *testing.T) {
	vim, err := exec.LookPath("vim")
	if err != nil {
		t.Skip("vim isn't installed")
	}
	m := newTestManager(t)
	opts := shellOptions(t)
	file := filepath.Join(opts.WorkDir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("first line\nsecond line\n"), 0o600))
	for i, kv := range opts.Env {
		if strings.HasPrefix(kv, "TERM=") {
			opts.Env[i] = "TERM=xterm-256color"
		}
	}
	id, err := m.Create(context.Background(), opts)
	require.NoError(t, err)

	watched := attachClient(t, m, id, 80, 24, AttachOptions{})
	watched.waitText(t, `\$$`)
	require.NoError(t, watched.a.Write([]byte("echo before vim\n")))
	watched.waitText(t, `before vim\n\$$`)
	require.NoError(t, watched.a.Write([]byte(vim+" -u NONE -N -n -i NONE "+file+"\n")))
	watched.waitFor(t, "vim on the alternate screen", func(s *Screen) bool {
		return s.AltActive() && strings.Contains(s.Text(), "second line")
	})
	watched.quiet(t)
	watched.detach(t)

	attached := attachClient(t, m, id, 80, 24, AttachOptions{})
	attached.waitFor(t, "vim restored", func(s *Screen) bool { return s.AltActive() })
	attached.quiet(t)
	requireSameScreen(t, watched, attached)

	// Vim takes input as it did: an edit, then quitting back to the normal screen.
	require.NoError(t, attached.a.Write([]byte("ggiedited \x1b:wq\r")))
	attached.waitFor(t, "the normal screen back", func(s *Screen) bool {
		return !s.AltActive() && strings.Contains(s.Text(), "before vim")
	})
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, "edited first line\nsecond line\n", string(data))
}
