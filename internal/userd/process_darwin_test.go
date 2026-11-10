package userd_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/userd"
)

func TestParsePSStats(t *testing.T) {
	stats := "    1     0 Ss    2048   1:02.50 root             /sbin/launchd\n" +
		"  500     1 R+       4 1:00:00,25 bob              /Applications/My App.app/Contents/MacOS/My App\n" +
		"  501     1 Z        0   0:00.00 bob              (zombie)\n" +
		"not a process\n"
	cmdlines := map[string]string{"500": "/Applications/My App.app/Contents/MacOS/My App --flag"}

	snapshots := userd.ParsePSStats(stats, cmdlines, 4096*1024)
	require.Len(t, snapshots, 3)

	require.Equal(t, userd.ProcessInfo{PID: 1, Name: "launchd", Exe: "/sbin/launchd", User: "root", Status: "S",
		MemRSS: 2048 * 1024, MemPercent: 50}, snapshots[0].Info)
	require.InDelta(t, 62.5, snapshots[0].CPUTime, 0.001)

	require.Equal(t, "My App", snapshots[1].Info.Name)
	require.Equal(t, "/Applications/My App.app/Contents/MacOS/My App", snapshots[1].Info.Exe)
	require.Equal(t, "/Applications/My App.app/Contents/MacOS/My App --flag", snapshots[1].Info.Cmdline)
	require.InDelta(t, 3600.25, snapshots[1].CPUTime, 0.001)

	require.Equal(t, "(zombie)", snapshots[2].Info.Name)
	require.Empty(t, snapshots[2].Info.Exe)
	require.Equal(t, "Z", snapshots[2].Info.Status)
}

func TestListProcessesSeesOtherUsers(t *testing.T) {
	snapshots, err := userd.ListProcesses()
	require.NoError(t, err)

	byPID := make(map[int32]userd.ProcessInfo, len(snapshots))
	for _, s := range snapshots {
		byPID[s.Info.PID] = s.Info
	}

	launchd := byPID[1]
	require.Equal(t, "launchd", launchd.Name)
	require.Equal(t, "root", launchd.User)
	require.NotZero(t, launchd.MemRSS)
	require.NotEmpty(t, launchd.Cmdline)

	self := byPID[int32(os.Getpid())]
	require.NotEmpty(t, self.Exe)
}
