package userd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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

func formatJournalLine(line []byte) (string, bool) {
	tsUsec, fields, err := parseJournalctlEntry(line)
	if err != nil {
		return "", false
	}
	return formatEntry(tsUsec, fields), true
}

const (
	unifiedLogPath          = "/usr/bin/log"
	unifiedLogDefaultWindow = time.Hour
	unifiedLogTimeLayout    = "2006-01-02 15:04:05"
	unifiedLogEntryLayout   = "2006-01-02 15:04:05.000000-0700"
)

func unifiedLogHistory(follow bool, tailN int64, since string, now time.Time) (time.Time, int64, bool) {
	if since != "" {
		return now.Add(-parseSinceDuration(since)), -1, true
	}
	if !follow {
		if tailN < 0 {
			tailN = 10
		}
		return now.Add(-unifiedLogDefaultWindow), tailN, true
	}
	if tailN > 0 {
		return now.Add(-unifiedLogDefaultWindow), tailN, true
	}
	return time.Time{}, 0, false
}

func unifiedLogShowArgs(service string, start time.Time) []string {
	args := []string{"show", "--style", "ndjson", "--start", start.Format(unifiedLogTimeLayout)}
	return append(args, unifiedLogPredicate(service)...)
}

func unifiedLogStreamArgs(service string) []string {
	return append([]string{"stream", "--style", "ndjson"}, unifiedLogPredicate(service)...)
}

func unifiedLogPredicate(service string) []string {
	if service == "" {
		return nil
	}
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(service) + `"`
	return []string{"--predicate", "process == " + quoted + " OR subsystem == " + quoted}
}

type unifiedLogEntry struct {
	Timestamp        string `json:"timestamp"`
	ProcessImagePath string `json:"processImagePath"`
	ProcessID        int64  `json:"processID"`
	EventMessage     string `json:"eventMessage"`
}

func formatUnifiedLogLine(line []byte, hostname string) (string, bool) {
	var entry unifiedLogEntry
	if err := json.Unmarshal(line, &entry); err != nil || entry.Timestamp == "" {
		return "", false
	}
	ts, err := time.Parse(unifiedLogEntryLayout, entry.Timestamp)
	if err != nil {
		return "", false
	}

	fields := map[string]string{"_HOSTNAME": hostname, "MESSAGE": entry.EventMessage}
	if entry.ProcessImagePath != "" {
		fields["SYSLOG_IDENTIFIER"] = filepath.Base(entry.ProcessImagePath)
	}
	if entry.ProcessID > 0 {
		fields["_PID"] = strconv.FormatInt(entry.ProcessID, 10)
	}

	return formatEntry(uint64(ts.UnixMicro()), fields), true //nolint:gosec
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
