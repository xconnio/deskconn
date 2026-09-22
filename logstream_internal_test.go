package deskconn

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// collectLogData reads envelopes from client until the stream closes (the
// device finished or errored), concatenating every logMsgData payload and
// ignoring pings.
func collectLogData(t *testing.T, client net.Conn, receiveKey []byte, timeout time.Duration) []byte {
	t.Helper()
	var collected []byte
	deadline := time.Now().Add(timeout)
	for {
		_ = client.SetReadDeadline(deadline)
		kind, plaintext, err := recvPortEnvelope(client, receiveKey)
		if err != nil {
			return collected
		}
		if kind == logMsgData {
			collected = append(collected, plaintext...)
		}
	}
}

func startLogsStream(t *testing.T) (client net.Conn, sendKey, receiveKey []byte) {
	t.Helper()
	c, server := net.Pipe()
	t.Cleanup(func() { _ = c.Close() })
	d := Deskconn{}
	go d.handleQUICLogsStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(c)
	require.NoError(t, err)
	return c, sendKey, receiveKey
}

func TestLogsInvalidSince(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Since: "notaduration", TailN: -1}), sendKey))

	data := collectLogData(t, client, receiveKey, 3*time.Second)
	require.Contains(t, string(data), "error:")
	require.Contains(t, string(data), "notaduration")
}

func TestLogsFileNotFound(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: "/nonexistent/path/file.log", TailN: -1}), sendKey))

	data := collectLogData(t, client, receiveKey, 3*time.Second)
	require.Contains(t, string(data), "error:")
}

func TestLogsFileEmpty(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	f, err := os.CreateTemp("", "logs-empty-*.log")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: f.Name(), TailN: -1}), sendKey))

	data := collectLogData(t, client, receiveKey, 3*time.Second)
	require.Equal(t, "-- No entries --\n", string(data))
}

func TestLogsFileContent(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	content := "line one\nline two\nline three\n"
	f, err := os.CreateTemp("", "logs-content-*.log")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	// tailN=0 reads from the beginning of the file.
	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: f.Name(), TailN: 0}), sendKey))

	data := collectLogData(t, client, receiveKey, 3*time.Second)
	require.Equal(t, content, string(data))
}

func TestLogsFileTailN(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	lines := "alpha\nbeta\ngamma\ndelta\nepsilon\n"
	f, err := os.CreateTemp("", "logs-tailn-*.log")
	require.NoError(t, err)
	_, err = f.WriteString(lines)
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: f.Name(), TailN: 2}), sendKey))

	got := string(collectLogData(t, client, receiveKey, 3*time.Second))
	require.Contains(t, got, "delta\n")
	require.Contains(t, got, "epsilon\n")
	require.NotContains(t, got, "alpha")
	require.NotContains(t, got, "beta")
	require.NotContains(t, got, "gamma")
}

func TestLogsFileFollow(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	f, err := os.CreateTemp("", "logs-follow-*.log")
	require.NoError(t, err)
	path := f.Name()
	_, err = f.WriteString("old content\n")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	// tailN=-1 with follow seeks to end; old content is skipped.
	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: path, Follow: true, TailN: -1}), sendKey))

	go func() {
		time.Sleep(100 * time.Millisecond)
		f2, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			return
		}
		defer f2.Close()
		_, _ = f2.WriteString("new line\n")
	}()

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	var received []byte
	for {
		kind, plaintext, err := recvPortEnvelope(client, receiveKey)
		require.NoError(t, err)
		if kind == logMsgData {
			received = append(received, plaintext...)
			if len(received) > 0 {
				break
			}
		}
	}
	require.Contains(t, string(received), "new line")
}

// TestLogsFollowIgnoresPing confirms a no-op logMsgPing doesn't disrupt an
// otherwise-normal follow session.
func TestLogsFollowIgnoresPing(t *testing.T) {
	client, sendKey, receiveKey := startLogsStream(t)

	f, err := os.CreateTemp("", "logs-ping-*.log")
	require.NoError(t, err)
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: path, Follow: true, TailN: -1}), sendKey))

	require.NoError(t, sendPortEnvelope(client, logMsgPing, nil, sendKey))

	go func() {
		time.Sleep(100 * time.Millisecond)
		f2, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			return
		}
		defer f2.Close()
		_, _ = f2.WriteString("still works\n")
	}()

	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, plaintext, err := recvPortEnvelope(client, receiveKey)
		require.NoError(t, err)
		if kind == logMsgData {
			require.Equal(t, "still works\n", string(plaintext))
			return
		}
	}
}

// TestLogsFollowStopsOnDisconnect confirms that closing the client during a
// --follow session actually stops the device's file watch, mirroring
// TestPortForwardClosesBackendOnClientDisconnect's spirit: a fsnotify.Watcher
// leak here would show up as the device goroutine never returning, which
// -race (run across the whole suite) would catch as a leaked goroutine
// holding the watcher open indefinitely if this regressed.
func TestLogsFollowStopsOnDisconnect(t *testing.T) {
	client, server := net.Pipe()
	d := Deskconn{}
	done := make(chan struct{})
	go func() {
		d.handleQUICLogsStream(server)
		close(done)
	}()

	sendKey, _, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	f, err := os.CreateTemp("", "logs-disconnect-*.log")
	require.NoError(t, err)
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: path, Follow: true, TailN: -1}), sendKey))

	require.NoError(t, client.Close())

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("device's log stream handler did not return after client disconnect")
	}
}

func TestLogsJournalNoEntries(t *testing.T) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("journalctl not available")
	}
	client, sendKey, receiveKey := startLogsStream(t)

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{TailN: 0}), sendKey))

	got := string(collectLogData(t, client, receiveKey, 5*time.Second))
	require.True(t,
		strings.Contains(got, "-- No entries --") || strings.HasPrefix(got, "error"),
		"unexpected journal response: %q", got)
}

func TestLogsJournalFakeService(t *testing.T) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("journalctl not available")
	}
	client, sendKey, receiveKey := startLogsStream(t)

	require.NoError(t, sendPortEnvelope(client, logMsgControl,
		mustJSON(logControlMsg{Source: "nonexistent-service-xyz123abc", TailN: 0}), sendKey))

	got := string(collectLogData(t, client, receiveKey, 5*time.Second))
	require.True(t,
		strings.Contains(got, "-- No entries --") || strings.HasPrefix(got, "error"),
		"unexpected journal response: %q", got)
}
