package deskconn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/xconnio/xconn-go"
)

type logSession struct {
	callerID uint64
	stop     chan struct{}
	once     sync.Once
}

func newLogSession(callerID uint64) *logSession {
	return &logSession{callerID: callerID, stop: make(chan struct{})}
}

func (ls *logSession) kill() {
	ls.once.Do(func() { close(ls.stop) })
}

// logSessions tracks active log streams keyed by streamID (unique per call).
type logSessions struct {
	mu   sync.Mutex
	sess map[uint64]*logSession
}

func newLogSessions() *logSessions {
	return &logSessions{sess: make(map[uint64]*logSession)}
}

func (l *logSessions) deleteIfSame(streamID uint64, ls *logSession) {
	l.mu.Lock()
	if l.sess[streamID] == ls {
		delete(l.sess, streamID)
	}
	l.mu.Unlock()
}

// replace atomically stores ls under streamID and returns any previous session.
func (l *logSessions) replace(streamID uint64, ls *logSession) *logSession {
	l.mu.Lock()
	old := l.sess[streamID]
	l.sess[streamID] = ls
	l.mu.Unlock()
	return old
}

func (l *logSessions) killAndDelete(streamID uint64) {
	l.mu.Lock()
	ls, ok := l.sess[streamID]
	delete(l.sess, streamID)
	l.mu.Unlock()
	if ok {
		ls.kill()
	}
}

// killAndDeleteByCaller kills all active streams that belong to the given callerID.
func (l *logSessions) killAndDeleteByCaller(callerID uint64) {
	l.mu.Lock()
	var toKill []*logSession
	for streamID, ls := range l.sess {
		if ls.callerID == callerID {
			delete(l.sess, streamID)
			toKill = append(toKill, ls)
		}
	}
	l.mu.Unlock()
	for _, ls := range toKill {
		ls.kill()
	}
}

func StreamLogs(session *xconn.Session, realm, source string, follow bool, tailN int64, since string) error {
	if since != "" {
		if _, err := time.ParseDuration(since); err != nil {
			return fmt.Errorf("invalid since value %q: %w", since, err)
		}
	}

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	SafeGo(func() {
		select {
		case <-sigChan:
			closeDone()
		case <-done:
		}
	})
	defer signal.Stop(sigChan)

	firstSent := false
	callResp := session.Call(ProcedureProxyLogs).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				return xconn.NewProgress(realm, source, follow, tailN, since)
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
			data, err := pr.ArgBytes(0)
			if err != nil {
				return
			}
			_, _ = os.Stdout.Write(data)
		}).Do()

	return callResp.Err
}

func (d *Deskconn) handleLogs(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	callerID := inv.Caller()

	if !inv.Progress() {
		streamID, err := inv.ArgUInt64(0)
		if err != nil {
			d.logs.killAndDeleteByCaller(callerID)
		} else {
			d.logs.killAndDelete(streamID)
		}
		return xconn.NewInvocationResult()
	}

	source, _ := inv.ArgString(0)
	follow, _ := inv.ArgBool(1)
	tailN, err := inv.ArgInt64(2)
	if err != nil {
		tailN = -1
	}
	since, _ := inv.ArgString(3)
	if since != "" {
		if _, parseErr := time.ParseDuration(since); parseErr != nil {
			return xconn.NewInvocationError(ErrInvalidArgument,
				fmt.Sprintf("invalid since value %q: %s", since, parseErr.Error()))
		}
	}
	streamID, err := inv.ArgUInt64(4)
	if err != nil {
		streamID = callerID
	}

	ls := newLogSession(callerID)
	if old := d.logs.replace(streamID, ls); old != nil {
		old.kill()
	}

	if filepath.IsAbs(source) {
		SafeGo(func() { d.streamFileLogs(ls, streamID, inv, source, follow, tailN) })
	} else {
		SafeGo(func() { d.streamJournalLogs(ctx, ls, streamID, inv, source, follow, tailN, since) })
	}

	return xconn.NewInvocationError(xconn.ErrNoResult)
}

func (d *Deskconn) streamJournalLogs(ctx context.Context, ls *logSession, streamID uint64, inv *xconn.Invocation,
	service string, follow bool, tailN int64, since string) {
	defer func() {
		d.logs.deleteIfSame(streamID, ls)
		select {
		case <-ls.stop:
		default:
			_ = inv.SendProgress(nil, nil)
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	SafeGo(func() {
		select {
		case <-ls.stop:
			cancel()
		case <-ctx.Done():
		}
	})

	args := journalctlArgs(service, follow, tailN, since)
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", err.Error()))}, nil)
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	if err := cmd.Start(); err != nil {
		_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", err.Error()))}, nil)
		return
	}

	sent := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		tsUsec, fields, parseErr := parseJournalctlEntry(line)
		if parseErr != nil {
			continue
		}

		sent = true
		_ = inv.SendProgress([]any{[]byte(formatEntry(tsUsec, fields))}, nil)
	}

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return
	}

	if err := scanner.Err(); err != nil {
		if stderr.Len() > 0 {
			_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String())))}, nil)
			return
		}
		_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", err.Error()))}, nil)
		return
	}

	if !sent {
		if stderr.Len() > 0 {
			_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String())))}, nil)
			return
		}
		if waitErr != nil {
			_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", waitErr.Error()))}, nil)
			return
		}
		_ = inv.SendProgress([]any{[]byte("-- No entries --\n")}, nil)
		return
	}

	if waitErr != nil && stderr.Len() > 0 && ctx.Err() == nil {
		_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String())))}, nil)
	}
}

func (d *Deskconn) streamFileLogs(ls *logSession, streamID uint64, inv *xconn.Invocation,
	path string, follow bool, tailN int64) {
	defer func() {
		d.logs.deleteIfSame(streamID, ls)
		select {
		case <-ls.stop:
		default:
			_ = inv.SendProgress(nil, nil)
		}
	}()

	f, err := os.Open(path)
	if err != nil {
		_ = inv.SendProgress([]any{[]byte(fmt.Sprintf("error: %s\n", err.Error()))}, nil)
		return
	}
	defer f.Close()

	n := tailN
	if n < 0 {
		if follow {
			// --follow with no --tail: start from end of file
			if _, seekErr := f.Seek(0, io.SeekEnd); seekErr != nil {
				return
			}
		} else {
			n = 10
		}
	}
	if n >= 0 {
		if offset, offsetErr := nthLineFromEnd(path, n); offsetErr == nil {
			_, _ = f.Seek(offset, io.SeekStart)
		}
	}

	if !follow {
		buf := make([]byte, 4096)
		sent := false
		for {
			select {
			case <-ls.stop:
				return
			default:
			}
			nr, readErr := f.Read(buf)
			if nr > 0 {
				sent = true
				_ = inv.SendProgress([]any{buf[:nr]}, nil)
			}
			if readErr != nil {
				if !sent {
					_ = inv.SendProgress([]any{[]byte("-- No entries --\n")}, nil)
				}
				return
			}
		}
	}

	watcher, watchErr := fsnotify.NewWatcher()
	if watchErr != nil {
		return
	}
	defer watcher.Close()
	if addErr := watcher.Add(path); addErr != nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, readErr := f.Read(buf)
		if nr > 0 {
			_ = inv.SendProgress([]any{buf[:nr]}, nil)
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return
		}
		select {
		case <-ls.stop:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				continue
			}
		case <-watcher.Errors:
			return
		}
	}
}

func parseSinceDuration(since string) time.Duration {
	d, _ := time.ParseDuration(since)
	return d
}

func nthLineFromEnd(path string, n int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0, nil
	}

	size := info.Size()

	// Don't count a trailing newline as its own line boundary.
	last := make([]byte, 1)
	if _, readErr := f.ReadAt(last, size-1); readErr == nil && last[0] == '\n' {
		size--
	}

	buf := make([]byte, 4096)
	linesFound := int64(0)
	pos := size

	for pos > 0 {
		readSize := int64(len(buf))
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize
		nr, readErr := f.ReadAt(buf[:readSize], pos)
		if readErr != nil && nr == 0 {
			break
		}
		for i := int64(nr) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				linesFound++
				if linesFound == n {
					return pos + i + 1, nil
				}
			}
		}
	}
	return 0, nil
}

func formatEntry(tsUsec uint64, fields map[string]string) string {
	ts := time.UnixMicro(int64(tsUsec)).Local() //nolint:gosec
	hostname := fields["_HOSTNAME"]
	ident := fields["SYSLOG_IDENTIFIER"]
	if ident == "" {
		ident = fields["_COMM"]
	}
	pid := fields["_PID"]
	msg := fields["MESSAGE"]

	tsStr := ts.Format("Jan 02 15:04:05.000000")
	if pid != "" {
		return fmt.Sprintf("%s %s %s[%s]: %s\n", tsStr, hostname, ident, pid, msg)
	}
	if ident != "" {
		return fmt.Sprintf("%s %s %s: %s\n", tsStr, hostname, ident, msg)
	}
	return fmt.Sprintf("%s %s: %s\n", tsStr, hostname, msg)
}

func journalctlArgs(service string, follow bool, tailN int64, since string) []string {
	args := []string{"--no-pager", "--output=json"}

	if service != "" {
		unitName := service
		if !strings.HasSuffix(unitName, ".service") {
			unitName += ".service"
		}
		args = append(args, "--unit="+unitName)
	}

	if since != "" {
		dur := parseSinceDuration(since)
		args = append(args, "--since="+time.Now().Add(-dur).Format(time.RFC3339Nano))
	} else if follow {
		if tailN > 0 {
			args = append(args, fmt.Sprintf("--lines=%d", tailN))
		} else {
			args = append(args, "--since="+time.Now().Format(time.RFC3339Nano))
		}
	} else {
		if tailN < 0 {
			tailN = 10
		}
		args = append(args, fmt.Sprintf("--lines=%d", tailN))
	}

	if follow {
		args = append(args, "--follow")
	}

	return args
}

func parseJournalctlEntry(line []byte) (uint64, map[string]string, error) {
	var fields map[string]string
	if err := json.Unmarshal(line, &fields); err != nil {
		return 0, nil, err
	}

	tsUsec := parseJournalTimestamp(fields)
	return tsUsec, fields, nil
}

func parseJournalTimestamp(fields map[string]string) uint64 {
	for _, key := range []string{"__REALTIME_TIMESTAMP", "_SOURCE_REALTIME_TIMESTAMP"} {
		if raw := fields[key]; raw != "" {
			if ts, err := strconv.ParseUint(raw, 10, 64); err == nil {
				return ts
			}
		}
	}
	return 0
}
