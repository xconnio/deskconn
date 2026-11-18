//go:build !windows

package common_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
)

func TestCfgDirectoryIsPrivate(t *testing.T) {
	tempHome(t)
	dir, err := common.CfgDirectory()
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestCfgDirectoryTightensExisting(t *testing.T) {
	home := tempHome(t)
	existing := filepath.Join(home, ".deskconn")
	require.NoError(t, os.Mkdir(existing, 0o700))
	require.NoError(t, os.Chmod(existing, 0o755)) //nolint:gosec // An install from before it was made private.
	require.NoError(t, os.WriteFile(filepath.Join(existing, "id_ed25519"), []byte("key"), 0o600))

	dir, err := common.CfgDirectory()
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	key, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
	require.NoError(t, err)
	require.Equal(t, "key", string(key), "tightening keeps what the directory holds")
}
