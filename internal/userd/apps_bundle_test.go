package userd_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/userd"
)

func TestBuildAppsMatchesBundlePath(t *testing.T) {
	const (
		safari  = "Safari"
		slack   = "Slack"
		handler = "Claude Code URL Handler"
		cryptex = "/System/Cryptexes/App/System/Applications/Safari.app/"
	)
	registry := map[string]userd.DesktopEntry{
		"/Applications/Safari.app/": {ID: safari, Name: safari},
		cryptex:                     {ID: safari, Name: safari},
		"/Users/me/Applications/" + handler + ".app/": {ID: handler, Name: handler},
		"/Applications/Slack.app/":                    {ID: slack, Name: slack},
	}
	helper := "Slack Helper (Renderer)"
	helperExe := "/Applications/Slack.app/Contents/Frameworks/" + helper + ".app/Contents/MacOS/" + helper
	procs := []userd.ProcessInfo{
		{PID: 1, Name: safari, Exe: cryptex + "Contents/MacOS/Safari"},
		{PID: 2, Name: slack, Exe: "/Applications/Slack.app/Contents/MacOS/Slack"},
		{PID: 3, Name: helper, Exe: helperExe},
		{PID: 4, Name: "claude", Exe: "/Users/me/.local/bin/claude"},
		{PID: 5, Name: "launchd", Exe: "/sbin/launchd"},
	}

	got := map[string][]int32{}
	for _, app := range userd.BuildApps(procs, registry) {
		got[app.ID] = app.PIDs
	}

	require.Equal(t, map[string][]int32{safari: {1}, slack: {2, 3}, "system": {4, 5}}, got)
}
