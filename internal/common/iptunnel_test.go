package common_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
)

const (
	hetznerDNS = "185.12.64.1"
	routeIP    = "203.0.113.10"
	routeHost  = routeIP + "/32"
	routeNet   = "10.1.0.0/16"
)

func TestParseResolvConfNameservers(t *testing.T) {
	data := []byte("# comment\nnameserver " + hetznerDNS + "\nsearch example.com\n" +
		"  nameserver   2a01:4ff:ff00::add:1 \nnameserver\n")
	require.Equal(t, []string{hetznerDNS, "2a01:4ff:ff00::add:1"}, common.ParseResolvConfNameservers(data))
}

func TestTunnelDNSServers(t *testing.T) {
	servers := []string{
		hetznerDNS,           // public: kept
		"127.0.0.53",         // systemd-resolved stub
		"192.168.0.1",        // private LAN
		"10.0.0.2",           // private
		"169.254.169.254",    // link-local
		"2a01:4ff:ff00::add", // IPv6
		"not-an-ip",
		" 8.8.8.8 ",
	}
	require.Equal(t, []string{hetznerDNS, "8.8.8.8"}, common.TunnelDNSServers(servers))
	require.Empty(t, common.TunnelDNSServers(nil))
}

func TestParseVPNRoute(t *testing.T) {
	valid := map[string]string{
		routeIP:             routeHost,
		" " + routeIP + " ": routeHost,
		routeHost:           routeHost,
		"10.1.2.3/16":       routeNet,
		"128.0.0.0/1":       "128.0.0.0/1",
	}
	for input, want := range valid {
		got, err := common.ParseVPNRoute(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}

	for _, input := range []string{"", "example.com", "default", "1.2.3.4/33", "0.0.0.0/0", "9.9.9.9/0",
		"2001:db8::1", "2001:db8::/32", "::ffff:1.2.3.4"} {
		_, err := common.ParseVPNRoute(input)
		require.Error(t, err, input)
	}
}

func TestParseVPNRoutes(t *testing.T) {
	routes, err := common.ParseVPNRoutes([]string{routeIP, "10.1.2.3/16", routeHost, routeNet})
	require.NoError(t, err)
	require.Equal(t, []string{routeHost, routeNet}, routes)

	routes, err = common.ParseVPNRoutes(nil)
	require.NoError(t, err)
	require.Empty(t, routes)

	_, err = common.ParseVPNRoutes([]string{routeIP, "nope"})
	require.Error(t, err)
}
