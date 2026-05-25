package deskconn_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

// collectLogs streams non-follow logs from source and returns all received bytes.
func collectLogs(t *testing.T, caller *xconn.Session, source string, tailN int64) ([]byte, error) {
	t.Helper()

	var mu sync.Mutex
	var collected []byte
	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	sent := false

	callResp := caller.Call(deskconn.ProcedureLogs).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress(source, false, tailN)
			}
			select {
			case <-done:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) == 0 {
				closeDone()
				return
			}
			data, _ := pr.ArgBytes(0)
			mu.Lock()
			collected = append(collected, data...)
			mu.Unlock()
		}).Do()

	return collected, callResp.Err
}

func TestLogsNoProgress(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedureLogs).Do()
	require.NoError(t, callResp.Err)
	require.Empty(t, callResp.Args())
}

func TestLogsInvalidSince(t *testing.T) {
	_, caller := setupDeskconn(t)
	sent := false
	callResp := caller.Call(deskconn.ProcedureLogs).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("", false, int64(-1), "notaduration")
			}
			<-ctx.Done()
			return xconn.NewFinalProgress()
		}).Do()
	require.Error(t, callResp.Err)
	require.Contains(t, callResp.Err.Error(), deskconn.ErrInvalidArgument)
}

// TestLogsFileNotFound verifies that streaming a non-existent file sends an error message.
func TestLogsFileNotFound(t *testing.T) {
	_, caller := setupDeskconn(t)
	data, err := collectLogs(t, caller, "/nonexistent/path/file.log", -1)
	require.NoError(t, err)
	require.Contains(t, string(data), "error:")
}

// TestLogsFileEmpty verifies that an empty file produces "-- No entries --".
func TestLogsFileEmpty(t *testing.T) {
	_, caller := setupDeskconn(t)

	f, err := os.CreateTemp("", "logs-empty-*.log")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	data, err := collectLogs(t, caller, f.Name(), -1)
	require.NoError(t, err)
	require.Equal(t, "-- No entries --\n", string(data))
}

// TestLogsFileContent verifies that all content of a non-empty file is streamed.
func TestLogsFileContent(t *testing.T) {
	_, caller := setupDeskconn(t)

	content := "line one\nline two\nline three\n"
	f, err := os.CreateTemp("", "logs-content-*.log")
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	// tailN=0 reads from the beginning of the file.
	data, err := collectLogs(t, caller, f.Name(), 0)
	require.NoError(t, err)
	require.Equal(t, content, string(data))
}

// TestLogsFileTailN verifies that only the last N lines are streamed.
func TestLogsFileTailN(t *testing.T) {
	_, caller := setupDeskconn(t)

	lines := "alpha\nbeta\ngamma\ndelta\nepsilon\n"
	f, err := os.CreateTemp("", "logs-tailn-*.log")
	require.NoError(t, err)
	_, err = f.WriteString(lines)
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	data, err := collectLogs(t, caller, f.Name(), 2)
	require.NoError(t, err)
	got := string(data)
	require.Contains(t, got, "delta\n")
	require.Contains(t, got, "epsilon\n")
	require.NotContains(t, got, "alpha")
	require.NotContains(t, got, "beta")
	require.NotContains(t, got, "gamma")
}

// TestLogsFileFollow verifies that new content written to a file is streamed in follow mode.
func TestLogsFileFollow(t *testing.T) {
	_, caller := setupDeskconn(t)

	f, err := os.CreateTemp("", "logs-follow-*.log")
	require.NoError(t, err)
	path := f.Name()
	_, err = f.WriteString("old content\n")
	require.NoError(t, err)
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	var received []byte
	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	sent := false

	callResp := caller.Call(deskconn.ProcedureLogs).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				// Write new content after a brief delay so the watcher is set up first.
				go func() {
					time.Sleep(100 * time.Millisecond)
					f2, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if openErr != nil {
						return
					}
					defer f2.Close()
					_, _ = f2.WriteString("new line\n")
				}()
				// tailN=-1 with follow seeks to end; old content is skipped.
				return xconn.NewProgress(path, true, int64(-1), "")
			}
			select {
			case <-done:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) == 0 {
				return
			}
			data, _ := pr.ArgBytes(0)
			received = append(received, data...)
			closeDone()
		}).Do()

	require.NoError(t, callResp.Err)
	require.Contains(t, string(received), "new line")
}

// TestLogsJournalNoEntries verifies that the journal path is exercised and returns
// a response without hanging. tailN=0 seeks to the tail so there are no entries to read.
func TestLogsJournalNoEntries(t *testing.T) {
	_, caller := setupDeskconn(t)

	data, err := collectLogs(t, caller, "", 0)
	require.NoError(t, err)
	got := string(data)
	require.True(t,
		strings.Contains(got, "-- No entries --") || strings.HasPrefix(got, "error"),
		"unexpected journal response: %q", got)
}

// TestLogsJournalFakeService verifies that filtering by a nonexistent service
// produces "-- No entries --".
func TestLogsJournalFakeService(t *testing.T) {
	_, caller := setupDeskconn(t)

	data, err := collectLogs(t, caller, "nonexistent-service-xyz123abc", 0)
	require.NoError(t, err)
	got := string(data)
	require.True(t,
		strings.Contains(got, "-- No entries --") || strings.HasPrefix(got, "error"),
		"unexpected journal response: %q", got)
}
