package common_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

const hetznerDNS = "185.12.64.1"

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
