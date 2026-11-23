package deskconn_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

const (
	officeName = "office"
	homeName   = "home"
	workName   = "work"
	hostURL    = "quic://host:18080"
)

func TestStandaloneMachines(t *testing.T) {
	dir := t.TempDir()

	machines, err := deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Empty(t, machines, "no config.yml yet")

	office := common.StandaloneMachine{Name: officeName, URL: hostURL, CertHash: "ab", PrivateKey: "cd",
		Username: "me"}
	require.NoError(t, deskconn.AddStandaloneMachine(dir, office))
	require.NoError(t, deskconn.AddStandaloneMachine(dir,
		common.StandaloneMachine{Name: homeName, URL: "quic://home:18080"}))
	require.ErrorContains(t, deskconn.AddStandaloneMachine(dir, office), "already exists")

	info, err := os.Stat(filepath.Join(dir, "config.yml"))
	require.NoError(t, err)
	// Windows has no POSIX permission bits: os.Chmod(0600) there only clears the read-only
	// attribute, so Stat reports a fixed 0666/0444 regardless of the mode requested - real
	// access control is ACL-based (NTFS inherits the user's own profile dir ACL) instead.
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0600), info.Mode().Perm(), "standalone machines hold private keys")
	}

	machines, err = deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Equal(t, []string{homeName, officeName}, []string{machines[0].Name, machines[1].Name})

	found, err := deskconn.FindStandaloneMachine(dir, officeName)
	require.NoError(t, err)
	require.Equal(t, office, found)

	require.NoError(t, deskconn.RemoveStandaloneMachine(dir, officeName))
	_, err = deskconn.FindStandaloneMachine(dir, officeName)
	require.ErrorIs(t, err, deskconn.ErrStandaloneMachineNotFound)
	require.ErrorIs(t, deskconn.RemoveStandaloneMachine(dir, officeName), deskconn.ErrStandaloneMachineNotFound)
}

func TestStandaloneMachineNamesManagedOnesRefused(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, deskconn.CacheMachines(dir,
		[]common.Machine{{Authid: "abcdef123", Name: officeName, Alias: workName}}))

	for _, name := range []string{officeName, workName, "abcdef123", "abc"} {
		err := deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: name, URL: hostURL})
		require.ErrorContains(t, err, "managed machine", name)
	}
	require.NoError(t, deskconn.AddStandaloneMachine(dir, common.StandaloneMachine{Name: "lab", URL: hostURL}))
}

func TestStandaloneMachinesShareConfigWithOtherSections(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"),
		[]byte("machines:\n  - authid: cloud\nprinting:\n  mode: accept\n"), 0600))

	require.NoError(t, deskconn.AddStandaloneMachine(dir,
		common.StandaloneMachine{Name: workName, URL: hostURL}))
	managed, err := common.MachinesFromCfg(dir)
	require.NoError(t, err)
	require.Len(t, managed, 1, "adding a standalone machine keeps the managed ones")

	// Refreshing the machine list and logging out keep the standalone machines.
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{{Authid: "refreshed"}}))
	require.NoError(t, deskconn.RemoveCredentialsFiles(dir))
	standalone, err := deskconn.StandaloneMachines(dir)
	require.NoError(t, err)
	require.Len(t, standalone, 1)

	data, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	require.NoError(t, err)
	require.Contains(t, string(data), "mode: accept")
}
