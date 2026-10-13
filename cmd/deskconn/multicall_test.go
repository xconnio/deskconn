package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/deskconn"
)

func TestMulticallArgs(t *testing.T) {
	args, ok := multicallArgs([]string{"/home/u/.local/lib/exec/deskconn-userd", "--standalone"}, userdName)
	require.True(t, ok, "by symlink name")
	require.Equal(t, []string{"--standalone"}, args)

	args, ok = multicallArgs([]string{"/home/u/.local/lib/exec/deskconn", deskconn.RootdName, "--socket", "s"},
		deskconn.RootdName)
	require.True(t, ok, "by first argument")
	require.Equal(t, []string{"--socket", "s"}, args)

	for _, argv := range [][]string{{"desk", "ls"}, {"deskconn", "ls"}} {
		_, ok = multicallArgs(argv, userdName)
		require.False(t, ok, argv)
		_, ok = multicallArgs(argv, deskconn.RootdName)
		require.False(t, ok, argv)
	}
}

func TestLinkProgramsReplacesOldDesk(t *testing.T) {
	binDir, execDir := t.TempDir(), t.TempDir()
	binary := filepath.Join(execDir, binaryName)
	require.NoError(t, os.WriteFile(binary, []byte("new"), 0o755))
	// Before the multicall binary, desk was a binary of its own.
	require.NoError(t, os.WriteFile(filepath.Join(binDir, cliName), []byte("old"), 0o755))

	require.NoError(t, linkPrograms(binDir, execDir))
	require.NoError(t, linkPrograms(binDir, execDir), "and again, on the next update")

	for _, link := range []string{
		filepath.Join(binDir, cliName),
		filepath.Join(execDir, userdName),
		filepath.Join(execDir, deskconn.RootdName),
	} {
		target, err := os.Readlink(link)
		require.NoError(t, err, link)
		require.Equal(t, binary, target)
	}
}
