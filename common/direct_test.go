package common_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

func TestEnsureDirectKeyIsStable(t *testing.T) {
	cfgDir := t.TempDir()

	authid, pub, priv, err := common.EnsureDirectKey(cfgDir)
	require.NoError(t, err)
	require.NotEmpty(t, authid)

	authid2, pub2, priv2, err := common.EnsureDirectKey(cfgDir)
	require.NoError(t, err)
	require.Equal(t, []string{authid, pub, priv}, []string{authid2, pub2, priv2})

	info, err := os.Stat(filepath.Join(cfgDir, "direct_ed25519"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}
