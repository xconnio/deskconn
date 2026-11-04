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
	exampleCom = "example.com"
	mailCom    = "mail." + exampleCom
	googleCom  = "google.com"
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
	routes, err := common.ParseVPNRoutes([]string{routeIP, "10.1.2.3/16", routeHost, routeNet,
		"Example.COM.", "*." + exampleCom, mailCom})
	require.NoError(t, err)
	require.Equal(t, []string{routeHost, routeNet}, routes.CIDRs)
	require.Equal(t, []string{exampleCom, mailCom}, routes.Domains)
	require.Equal(t, []string{routeHost, routeNet, exampleCom, mailCom}, routes.All())
	require.False(t, routes.Empty())

	routes, err = common.ParseVPNRoutes(nil)
	require.NoError(t, err)
	require.True(t, routes.Empty())

	for _, bad := range []string{"nope", "2001:db8::1", "0.0.0.0/0", "1.2.3.4/40", "1.2.3", "-a.com", "a..com"} {
		_, err = common.ParseVPNRoutes([]string{routeIP, bad})
		require.Error(t, err, bad)
	}
}

func TestParseVPNDomain(t *testing.T) {
	for input, want := range map[string]string{
		googleCom:            googleCom,
		" WWW.Google.Com. ":  "www.google.com",
		"*.googleapis.com":   "googleapis.com",
		"_dmarc.example.org": "_dmarc.example.org",
		"xn--bcher-kva.de":   "xn--bcher-kva.de",
	} {
		got, err := common.ParseVPNDomain(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}
	for _, input := range []string{"", "localhost", "1.2.3.4", "a b.com", "-x.com", "x-.com", "a..com",
		"--route=x.com", "~x.com", "x.com/24"} {
		_, err := common.ParseVPNDomain(input)
		require.Error(t, err, input)
	}
}

func TestMatchVPNDomain(t *testing.T) {
	domains := []string{googleCom, "example.org"}
	require.True(t, common.MatchVPNDomain("google.com.", domains))
	require.True(t, common.MatchVPNDomain("WWW.Google.com.", domains))
	require.True(t, common.MatchVPNDomain("a.b.example.org", domains))
	require.False(t, common.MatchVPNDomain("notgoogle.com.", domains))
	require.False(t, common.MatchVPNDomain("google.com.evil.net.", domains))
	require.False(t, common.MatchVPNDomain("com.", domains))
}
