//go:build linux

package common_test

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

func TestVPNHelperClientAlive(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "helper.sock")
	listener, err := net.Listen("unixpacket", socketPath)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()

	client, err := common.DialVPNHelper(socketPath)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	server := <-accepted
	require.NotNil(t, server)

	require.True(t, client.Alive())

	require.NoError(t, server.Close())
	require.False(t, client.Alive())
}
