//go:build linux || darwin

package userd_test

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
)

// cliTerminal is desk shell's attachment, to a daemon under test, with keys typed into it
// and the terminal's output collected.
type cliTerminal struct {
	keys   *io.PipeWriter
	output *lockedBuffer
	done   chan struct{}

	end     deskconn.TerminalEnd
	session string
	code    int
	err     error

	mu      sync.Mutex
	daemon  *userd.Deskconn
	servers []net.Conn // the machine's end of each connection
	offline bool       // the machine can't be reached
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func runCLITerminal(t *testing.T, d *userd.Deskconn, req common.TerminalControlMsg) *cliTerminal {
	t.Helper()
	keys, input := io.Pipe()
	c := &cliTerminal{keys: input, output: &lockedBuffer{}, done: make(chan struct{}), daemon: d}
	dial := func(ctx context.Context, req common.TerminalControlMsg) (
		*deskconn.TerminalStream, common.TerminalControlMsg, error) {
		c.mu.Lock()
		if c.offline {
			c.mu.Unlock()
			return nil, common.TerminalControlMsg{}, &deskconn.ConnectionError{Err: errors.New("offline")}
		}
		client, server := net.Pipe()
		c.servers = append(c.servers, server)
		d := c.daemon
		c.mu.Unlock()
		go d.ServeTerminalStream(server, terminalOwner) //nolint:contextcheck // serves as long as the stream
		return deskconn.StartTerminal(ctx, client, func() { _ = client.Close() }, req)
	}
	a := deskconn.NewTerminalAttachment(dial, keys, c.output, 80, 24)
	go func() {
		defer close(c.done)
		c.end, c.session, c.code, c.err = a.Run(context.Background(), req)
	}()
	t.Cleanup(func() {
		// Still attached, it detaches.
		go func() { _, _ = input.Write([]byte("\x1dd")) }()
		select {
		case <-c.done:
		case <-time.After(terminalWait):
			t.Error("the attachment doesn't end")
		}
		_ = input.Close()
	})
	return c
}

func (c *cliTerminal) typeKeys(t *testing.T, keys string) {
	t.Helper()
	_, err := c.keys.Write([]byte(keys))
	require.NoError(t, err)
}

func (c *cliTerminal) waitFor(t *testing.T, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	var match []string
	require.Eventually(t, func() bool {
		match = re.FindStringSubmatch(c.output.String())
		return match != nil
	}, terminalWait, 20*time.Millisecond, "waiting for %q in %q", pattern, c.output)
	return match[len(match)-1]
}

func (c *cliTerminal) pid(t *testing.T, marker string) int {
	t.Helper()
	c.typeKeys(t, "echo "+marker+"=$$\n")
	pid, err := strconv.Atoi(c.waitFor(t, marker+`=(\d+)\r?\n`))
	require.NoError(t, err)
	return pid
}

func (c *cliTerminal) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(terminalWait):
		t.Fatal("the attachment doesn't end")
	}
}

func TestCLICreateDetachResume(t *testing.T) {
	d, _ := terminalDaemon(t)
	first := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpCreate})
	pid := first.pid(t, "created")
	first.typeKeys(t, "printf '\\033[?1049h\\033[?2004hin full screen'\n")
	first.waitFor(t, "in full screen")

	first.typeKeys(t, "\x1dd")
	first.wait(t)
	require.NoError(t, first.err)
	require.Equal(t, deskconn.TerminalDetached, first.end)
	require.True(t, strings.HasSuffix(first.output.String(), "\x1b[?1049l\x1b[?2004l\x1b[?25h\x1b[0m\x1b(B\x1b)B\x0f"),
		"the terminal is left on its normal screen, the session's modes off: %q", first.output)
	sessions := listTerminals(t, d, terminalOwner)
	require.Len(t, sessions, 1)
	require.Equal(t, first.session, sessions[0].ID)
	require.False(t, sessions[0].Attached)

	// Resuming restores the screen, and the same shell goes on.
	second := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: first.session})
	second.waitFor(t, `created=`+strconv.Itoa(pid))
	second.waitFor(t, `\x1b\[\?1049h`)
	require.Equal(t, pid, second.pid(t, "resumed"))

	// The shell exiting ends the attachment, and the session.
	second.typeKeys(t, "exit 3\n")
	second.wait(t)
	require.NoError(t, second.err)
	require.Equal(t, deskconn.TerminalExited, second.end)
	require.Equal(t, 3, second.code)
	require.Empty(t, listTerminals(t, d, terminalOwner))
}

func TestCLIReconnects(t *testing.T) {
	d, _ := terminalDaemon(t)
	c := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpCreate})
	pid := c.pid(t, "before")
	c.typeKeys(t, "for i in $(seq 1 100); do echo history-$i; done\n")
	c.waitFor(t, `history-100\r\n`)

	// The network drops: the machine's end of the connection goes.
	c.mu.Lock()
	_ = c.servers[0].Close()
	c.mu.Unlock()
	// Attached again once the screen is redrawn: keys typed before that are dropped.
	c.waitFor(t, `connection lost; reconnecting[^\x00]*\x1b\[2J`)
	require.Equal(t, pid, c.pid(t, "after"), "to the same session")
	c.mu.Lock()
	require.Len(t, c.servers, 2)
	c.mu.Unlock()
	require.Equal(t, 1, strings.Count(c.output.String(), "history-50\r\n"),
		"history the terminal has isn't sent again")

	c.typeKeys(t, "exit\n")
	c.wait(t)
	require.NoError(t, c.err)
	require.Equal(t, deskconn.TerminalExited, c.end)
	require.Zero(t, c.code)
}

func TestCLIReconnectToEndedSession(t *testing.T) {
	d, _ := terminalDaemon(t)
	c := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpCreate})
	c.pid(t, "shell")
	c.mu.Lock()
	c.offline = true
	_ = c.servers[0].Close()
	c.mu.Unlock()
	ts, _, err := openTerminal(t, d, terminalOwner, common.TerminalControlMsg{Op: common.TerminalOpTerminate,
		Session: c.waitSession(t, d)})
	require.NoError(t, err)
	ts.Close()
	c.mu.Lock()
	c.offline = false
	c.mu.Unlock()
	c.wait(t)
	require.ErrorContains(t, c.err, "has ended")
}

// waitSession waits for the only session to be detached, and returns its ID.
func (c *cliTerminal) waitSession(t *testing.T, d *userd.Deskconn) string {
	t.Helper()
	var id string
	require.Eventually(t, func() bool {
		sessions := listTerminals(t, d, terminalOwner)
		if len(sessions) == 1 && !sessions[0].Attached {
			id = sessions[0].ID
		}
		return id != ""
	}, terminalWait, 20*time.Millisecond)
	return id
}

func TestCLITakeover(t *testing.T) {
	d, _ := terminalDaemon(t)
	first := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpCreate})
	pid := first.pid(t, "first")
	session := listTerminals(t, d, terminalOwner)[0].ID

	refused := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: session})
	refused.wait(t)
	require.True(t, deskconn.IsTerminalError(refused.err, common.TerminalErrAttached), "%v", refused.err)

	taker := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpAttach, Session: session,
		Takeover: true})
	first.wait(t)
	require.NoError(t, first.err)
	require.Equal(t, deskconn.TerminalDisplaced, first.end)
	require.Equal(t, pid, taker.pid(t, "taker"))
}

func TestCLIResumeMissingSession(t *testing.T) {
	d, _ := terminalDaemon(t)
	c := runCLITerminal(t, d, common.TerminalControlMsg{Op: common.TerminalOpAttach,
		Session: strings.Repeat("0", 32)})
	c.wait(t)
	require.True(t, deskconn.IsTerminalError(c.err, common.TerminalErrNotFound), "%v", c.err)
	require.Empty(t, listTerminals(t, d, terminalOwner), "no session is created")
}
