package userd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/userd"
)

func TestUnifiedLogHistory(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	cases := []struct {
		follow   bool
		tailN    int64
		since    string
		start    time.Time
		keep     int64
		withShow bool
	}{
		{false, -1, "", now.Add(-time.Hour), 10, true},
		{false, 25, "", now.Add(-time.Hour), 25, true},
		{false, 5, "10m", now.Add(-10 * time.Minute), -1, true},
		{true, -1, "", time.Time{}, 0, false},
		{true, 20, "", now.Add(-time.Hour), 20, true},
		{true, -1, "30s", now.Add(-30 * time.Second), -1, true},
	}
	for _, c := range cases {
		start, keep, ok := userd.UnifiedLogHistory(c.follow, c.tailN, c.since, now)
		require.Equal(t, c.withShow, ok, c)
		require.Equal(t, c.keep, keep, c)
		require.True(t, start.Equal(c.start), c)
	}
}

func TestUnifiedLogArgs(t *testing.T) {
	start := time.Date(2026, 11, 4, 9, 5, 7, 0, time.Local)
	require.Equal(t, []string{"show", "--style", "ndjson", "--start", "2026-11-04 09:05:07"},
		userd.UnifiedLogShowArgs("", start))
	require.Equal(t,
		[]string{"stream", "--style", "ndjson", "--predicate", `process == "a\"b" OR subsystem == "a\"b"`},
		userd.UnifiedLogStreamArgs(`a"b`))
}

func TestFormatUnifiedLogLine(t *testing.T) {
	line := []byte(`{"timestamp":"2026-11-04 10:15:01.123456+0000",` +
		`"processImagePath":"/usr/libexec/sshd-keygen-wrapper","processID":42,"eventMessage":"hello"}`)
	got, ok := userd.FormatUnifiedLogLine(line, "mac-test")
	require.True(t, ok)
	ts := time.Date(2026, 11, 4, 10, 15, 1, 123456000, time.UTC).Local().Format("Jan 02 15:04:05.000000")
	require.Equal(t, ts+" mac-test sshd-keygen-wrapper[42]: hello\n", got)

	for _, bad := range []string{`Filtering the log data using "process == x"`, `{"count":3}`, `{"timestamp":"nope"}`} {
		_, ok := userd.FormatUnifiedLogLine([]byte(bad), "h")
		require.False(t, ok, bad)
	}
}
