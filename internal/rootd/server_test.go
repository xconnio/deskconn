//go:build linux

package rootd_test

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/rootd"
)

// serve starts a Server for one client on a fresh socket, the way runRootd does, and returns
// that client and a channel closed once Serve returns.
func serve(t *testing.T) (*common.VPNHelperClient, <-chan struct{}) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "rootd.sock")
	listener, err := net.Listen("unixpacket", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		server := &rootd.Server{}
		server.Serve(conn.(*net.UnixConn))
	}()

	client, err := common.DialVPNHelper(socketPath)
	require.NoError(t, err)
	return client, done
}

func TestServerReturnsErrors(t *testing.T) {
	client, _ := serve(t)
	defer func() { _ = client.Close() }()

	// Fails reading the sysctl, before writing anything, so this is safe even as root.
	_, err := client.SetSysctl("deskconn.test.nonexistent", "1")
	require.ErrorContains(t, err, "/proc/sys/deskconn/test/nonexistent")
	require.True(t, client.Alive())
}

func TestServerRejectsInvalidTunnelRoutes(t *testing.T) {
	client, _ := serve(t)
	defer func() { _ = client.Close() }()

	// Rejected before "ip" runs, so this is safe even as root.
	for _, route := range []string{"0.0.0.0/0", "2001:db8::/32", "not-a-route", "default"} {
		require.ErrorContains(t, client.AddTunnelRoute(route, "lo"), "invalid route")
		require.ErrorContains(t, client.DelTunnelRoute(route, "lo"), "invalid route")
	}
	require.True(t, client.Alive())
}

func TestServerRejectsInvalidDNSDomains(t *testing.T) {
	client, _ := serve(t)
	defer func() { _ = client.Close() }()

	// Rejected before resolvectl runs or resolv.conf is touched, so this is safe even as root.
	for _, domain := range []string{"-x.com", "Example.com", "example", "a b.com", "1.2.3.4"} {
		_, err := client.SetLinkDNSDomains("lo", []string{"1.1.1.1"}, []string{domain})
		require.ErrorContains(t, err, "invalid dns domain")
	}
	require.True(t, client.Alive())
}

func TestServerStopsWhenClientDisconnects(t *testing.T) {
	client, done := serve(t)
	require.NoError(t, client.Close())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the client disconnected")
	}
}
