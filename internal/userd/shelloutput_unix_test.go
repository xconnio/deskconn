//go:build linux || darwin

package userd_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
)

func TestShellOutputSurvivesDelayedReader(t *testing.T) {
	literal := `spaces ; $(printf injected) '$HOME' "quoted"`
	large := strings.Repeat("terminal output ", 1024)
	for _, tc := range []struct {
		name    string
		command string
		args    []string
		output  string
		code    int
	}{
		{"literal arguments", "printf", []string{"%s", literal}, literal, 0},
		{"stderr and failure", "sh", []string{"-c", `printf '%s' "$1" >&2; exit 7`, "sh", literal}, literal, 7},
		{"multiple reads", "printf", []string{"%s", large}, large, 0},
		{"empty output", "true", nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := userd.NewInteractiveShellSession()
			transport := &userd.FakeShellTransport{}
			id, _, _, startReader, err := sessions.BeginShellSession(common.ShellControlMsg{
				Op: common.ShellOpSize, Cols: 80, Rows: 24, Command: tc.command, Args: tc.args,
			}, transport)
			require.NoError(t, err)
			t.Cleanup(func() { sessions.CleanupShellID(id) })

			// The real reader also starts after the shell acknowledgement is sent.
			// Give a short command time to exit before any output is read.
			time.Sleep(100 * time.Millisecond)
			startReader()
			require.Eventually(t, func() bool {
				return !sessions.HasSession(id)
			}, 3*time.Second, 10*time.Millisecond)

			require.Equal(t, tc.output, string(bytes.Join(transport.Written(), nil)))
			code, reported := transport.ExitCode()
			require.True(t, reported, "the session must report its exit status")
			require.Equal(t, tc.code, code)
		})
	}
}
