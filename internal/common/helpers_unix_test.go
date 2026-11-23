//go:build !windows

package common_test

import (
	"os"
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
