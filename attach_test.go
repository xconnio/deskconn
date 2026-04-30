package deskconn_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestCloudURIDefault(t *testing.T) {
	orig, wasSet := os.LookupEnv("DESKCONN_CLOUD_URI")
	require.NoError(t, os.Unsetenv("DESKCONN_CLOUD_URI"))
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("DESKCONN_CLOUD_URI", orig)
		}
	})

	require.Equal(t, "wss://api.deskconn.com/ws", deskconn.CloudURI())
}

func TestCloudURIFromEnv(t *testing.T) {
	custom := "wss://custom.example.com/ws"
	t.Setenv("DESKCONN_CLOUD_URI", custom)

	uri := deskconn.CloudURI()
	require.Equal(t, custom, uri)
}
