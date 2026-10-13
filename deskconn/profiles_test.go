package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
)

func TestProfiles(t *testing.T) {
	dir := t.TempDir()

	profiles, err := deskconn.Profiles(dir)
	require.NoError(t, err)
	require.Empty(t, profiles, "no config.yml yet")

	office := common.Profile{Name: "office", URL: "quic://host:18080", CertHash: "ab", PrivateKey: "cd", AuthID: "me"}
	require.NoError(t, deskconn.AddProfile(dir, office))
	require.NoError(t, deskconn.AddProfile(dir, common.Profile{Name: "home", URL: "quic://home:18080"}))
	require.ErrorContains(t, deskconn.AddProfile(dir, office), "already exists")

	info, err := os.Stat(filepath.Join(dir, "config.yml"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm(), "profiles hold private keys")

	profiles, err = deskconn.Profiles(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"home", "office"}, []string{profiles[0].Name, profiles[1].Name})

	found, err := deskconn.FindProfile(dir, "office")
	require.NoError(t, err)
	require.Equal(t, office, found)

	require.NoError(t, deskconn.RemoveProfile(dir, "office"))
	_, err = deskconn.FindProfile(dir, "office")
	require.ErrorIs(t, err, deskconn.ErrProfileNotFound)
	require.ErrorIs(t, deskconn.RemoveProfile(dir, "office"), deskconn.ErrProfileNotFound)
}

func TestProfilesShareConfigWithOtherSections(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"),
		[]byte("machines:\n  - authid: cloud\nprinting:\n  mode: accept\n"), 0600))

	require.NoError(t, deskconn.AddProfile(dir, common.Profile{Name: "work", URL: "quic://host:18080"}))
	machines, err := common.MachinesFromCfg(dir)
	require.NoError(t, err)
	require.Len(t, machines, 1, "adding a profile keeps the machines")

	// Refreshing the machine list and logging out keep the profiles.
	require.NoError(t, deskconn.CacheMachines(dir, []common.Machine{{Authid: "refreshed"}}))
	require.NoError(t, deskconn.RemoveCredentialsFiles(dir))
	profiles, err := deskconn.Profiles(dir)
	require.NoError(t, err)
	require.Len(t, profiles, 1)

	data, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	require.NoError(t, err)
	require.Contains(t, string(data), "mode: accept")
}
