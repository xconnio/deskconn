package deskconn_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/deskconn"
)

func TestIsRemotePath(t *testing.T) {
	require.True(t, deskconn.IsRemotePath("office:/tmp/file"))
	require.False(t, deskconn.IsRemotePath("file"))
	if runtime.GOOS == "windows" {
		require.False(t, deskconn.IsRemotePath("C:/Users/me/file"))
		require.True(t, deskconn.IsRemotePath("office:C:/Users/me/file"))
	}
}
