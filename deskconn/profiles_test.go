package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/deskconn"
)

func TestProfiles(t *testing.T) {
	dir := t.TempDir()

	profiles, err := deskconn.Profiles(dir)
	require.NoError(t, err)
	require.Empty(t, profiles, "no profiles file yet")

	office := deskconn.Profile{Name: "office", URL: "quic://host:18080", CertHash: "ab", PrivateKey: "cd", AuthID: "me"}
	require.NoError(t, deskconn.AddProfile(dir, office))
	require.NoError(t, deskconn.AddProfile(dir, deskconn.Profile{Name: "home", URL: "quic://home:18080"}))
	require.ErrorContains(t, deskconn.AddProfile(dir, office), "already exists")

	info, err := os.Stat(filepath.Join(dir, "profiles.yml"))
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
