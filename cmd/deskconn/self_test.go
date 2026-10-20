package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateExecStart(t *testing.T) {
	const execDir = "/home/u/.local/lib/exec"

	for _, old := range []string{"deskconn-userd", "deskconnd"} {
		unit := "[Service]\nExecStart=" + execDir + "/" + old + "\nRestart=always\n"
		require.Equal(t, "[Service]\nExecStart="+execDir+"/deskconn serve --mode user\nRestart=always\n",
			migrateExecStart(unit, execDir), old)
	}

	dropIn := "[Service]\nExecStart=\nExecStart=" + execDir + "/deskconn-userd --standalone --user a:b\n"
	require.Equal(t, "[Service]\nExecStart=\nExecStart="+execDir+"/deskconn serve --mode standalone --user a:b\n",
		migrateExecStart(dropIn, execDir), "--standalone becomes the standalone mode")

	dropIn = "[Service]\nExecStart=\nExecStart=" + execDir + "/deskconn-userd --user a:b --standalone\n"
	require.Equal(t, "[Service]\nExecStart=\nExecStart="+execDir+"/deskconn serve --mode standalone --user a:b\n",
		migrateExecStart(dropIn, execDir), "wherever the flag is")

	current := "[Service]\nExecStart=" + execDir + "/deskconn serve --mode user\n"
	require.Equal(t, current, migrateExecStart(current, execDir), "already migrated")
}
