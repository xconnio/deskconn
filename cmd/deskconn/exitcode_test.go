package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/deskconn"
)

func TestExitStatus(t *testing.T) {
	child := exec.Command("sh", "-c", "exit 4")
	if runtime.GOOS == goosWindows {
		child = exec.Command("cmd.exe", "/c", "exit 4")
	}
	childErr := child.Run()
	require.Error(t, childErr)

	for name, tc := range map[string]struct {
		err    error
		status int
		report bool
	}{
		"failure":          {errors.New("boom"), exitFailure, true},
		"usage":            {&usageError{msg: "bad flag"}, exitUsage, true},
		"connection":       {&deskconn.ConnectionError{Err: errors.New("offline")}, exitConnection, true},
		"wrapped conn":     {fmt.Errorf("ls: %w", &deskconn.ConnectionError{Err: errors.New("lost")}), exitConnection, true},
		"remote command":   {&deskconn.ExitError{Code: 42}, 42, false},
		"child process":    {childErr, 4, false},
		"already reported": {errAlreadyReported, exitFailure, false},
		"remote too big":   {&deskconn.ExitError{Code: 256}, wideExitStatus(256), false},
		"remote wide":      {&deskconn.ExitError{Code: 1000}, wideExitStatus(1000), false},
	} {
		t.Run(name, func(t *testing.T) {
			status, report := exitStatus(tc.err)
			require.Equal(t, tc.status, status)
			require.Equal(t, tc.report, report)
		})
	}
}

// wideExitStatus is what a remote exit code too wide for 8 bits becomes here.
func wideExitStatus(code int) int {
	if runtime.GOOS == goosWindows {
		return code
	}
	return exitFailure
}
