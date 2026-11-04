package common

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
)

// VPNOpenFrame is the client's first message on a VPN data channel.
type VPNOpenFrame struct {
	Type string `json:"type"`
}

// VPNReadyFrame is the server's reply once tunnel/NAT setup on its side has
// succeeded and it's safe for the client to start routing traffic into it.
type VPNReadyFrame struct {
	Type       string `json:"type"`
	ServerIP   string `json:"server_ip"`
	ClientCIDR string `json:"client_cidr"`
	MTU        int    `json:"mtu"`

	// DNS lists the exit node's own upstream resolvers, for the client to
	// send its lookups to through the tunnel (see TunnelDNSServers). Empty
	// from an exit node that predates it, or has none usable.
	DNS []string `json:"dns,omitempty"`
}

// TunnelDNSServers returns the entries of servers usable as DNS resolvers
// across the tunnel: public IPv4 addresses only. Loopback ones (e.g.
// systemd-resolved's 127.0.0.53 stub) only mean something on the exit node
// itself, and private/link-local ones could collide with the client's own
// LAN and get routed there instead of into the tunnel; IPv6 is blocked
// while the tunnel is up.
func TunnelDNSServers(servers []string) []string {
	var usable []string
	for _, s := range servers {
		ip := net.ParseIP(strings.TrimSpace(s)).To4()
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Equal(net.IPv4bcast) {
			continue
		}
		usable = append(usable, ip.String())
	}
	return usable
}

// ParseVPNRoute normalizes route, an IPv4 address or CIDR range, to the network form "ip route"
// accepts: 203.0.113.10 becomes 203.0.113.10/32 and 10.1.2.3/16 becomes 10.1.0.0/16. IPv6 is
// rejected since the tunnel only carries IPv4, and a /0 since it would replace the machine's
// default route, which a full tunnel deliberately leaves alone (see defaultHalves in rootd).
func ParseVPNRoute(route string) (string, error) {
	route = strings.TrimSpace(route)
	prefix, err := netip.ParsePrefix(route)
	if err != nil {
		addr, aerr := netip.ParseAddr(route)
		if aerr != nil {
			return "", fmt.Errorf("invalid route %q: want an IPv4 address or CIDR range", route)
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("invalid route %q: the vpn only carries IPv4", route)
	}
	if prefix.Bits() == 0 {
		return "", fmt.Errorf("invalid route %q: leave out --route to tunnel all traffic", route)
	}
	return prefix.Masked().String(), nil
}

// ParseVPNDomain normalizes domain for a route by name: lowercased, without a trailing dot or a
// leading "*." (a domain always covers its subdomains, see MatchVPNDomain). It must be a
// hostname with at least two labels, the last one not all digits, so it can't be mistaken for
// an address, and so it's safe to hand to resolvectl.
func ParseVPNDomain(domain string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimSuffix(strings.TrimPrefix(d, "*."), ".")
	invalid := fmt.Errorf("invalid route %q: want an IPv4 address, CIDR range or domain", domain)
	if len(d) > 253 || !strings.Contains(d, ".") {
		return "", invalid
	}
	labels := strings.Split(d, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalid
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", invalid
			}
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", invalid
	}
	return d, nil
}

// MatchVPNDomain reports whether name (as asked in a DNS query) is one of domains or a
// subdomain of one.
func MatchVPNDomain(name string, domains []string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, d := range domains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// VPNRoutes is what "desk vpn connect --route" tunnels instead of all traffic: address ranges
// as ParseVPNRoute normalizes them, and domains (with their subdomains) as ParseVPNDomain does.
type VPNRoutes struct {
	CIDRs   []string
	Domains []string
}

// Empty reports whether no routes were given, i.e. all traffic goes through the tunnel.
func (r VPNRoutes) Empty() bool {
	return len(r.CIDRs) == 0 && len(r.Domains) == 0
}

// All returns every route, ranges first.
func (r VPNRoutes) All() []string {
	return append(append([]string{}, r.CIDRs...), r.Domains...)
}

// ParseVPNRoutes sorts routes into ranges and domains, normalizing each and dropping
// duplicates. Anything that looks like an address is parsed as one, so an IPv6 address or a
// /0 is an error rather than a domain.
func ParseVPNRoutes(routes []string) (VPNRoutes, error) {
	var parsed VPNRoutes
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		trimmed := strings.TrimSpace(route)
		_, perr := netip.ParsePrefix(trimmed)
		_, aerr := netip.ParseAddr(trimmed)
		isAddr := perr == nil || aerr == nil || strings.Contains(trimmed, ":") || strings.Contains(trimmed, "/")

		var normalized string
		var err error
		if isAddr {
			normalized, err = ParseVPNRoute(trimmed)
		} else {
			normalized, err = ParseVPNDomain(trimmed)
		}
		if err != nil {
			return VPNRoutes{}, err
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		if isAddr {
			parsed.CIDRs = append(parsed.CIDRs, normalized)
		} else {
			parsed.Domains = append(parsed.Domains, normalized)
		}
	}
	return parsed, nil
}

// ParseResolvConfNameservers returns the addresses of the "nameserver"
// lines in a resolv.conf.
func ParseResolvConfNameservers(data []byte) []string {
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	return servers
}

// PumpTUNToChannel reads raw IP packets from tun and sends each as one
// binary message, blocking on channel's own backpressure rather than
// buffering unboundedly. Shared by both server and client.
func PumpTUNToChannel(tun *os.File, channel MessageChannel, done <-chan struct{}) {
	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(vpnSendBufferLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	buf := make([]byte, 65536)
	for {
		n, err := tun.Read(buf)
		if err != nil {
			select {
			case <-done:
			default:
				log.Debugf("iptunnel: tun read failed: %v", err)
			}
			return
		}
		if n == 0 {
			continue
		}

		// n is non-negative by construction (checked above).
		for channel.BufferedAmount()+uint64(n) > vpnSendBufferHigh { //nolint:gosec
			select {
			case <-sendReady:
			case <-done:
				return
			}
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if err := channel.Send(pkt); err != nil {
			return
		}
	}
}
