//go:build linux

package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go/webrtc"
)

// pinTargets returns the addresses that must be pinned to the current
// default route before it's replaced: the WebRTC peer (so the tunnel
// doesn't route into itself) and the cloud relay (so the machine's own
// control connection survives the switch).
func pinTargets(ctx context.Context, session *webrtc.WebRTCSession) []string {
	var ips []string

	if address, err := session.Connection().SelectedRemoteAddress(ctx); err == nil && address != "" {
		ips = append(ips, address)
	}

	if host, _, err := net.SplitHostPort(common.CloudQUICAddress()); err == nil {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, rerr := net.DefaultResolver.LookupIP(resolveCtx, "ip", host)
		cancel()
		if rerr != nil {
			log.Debugf("iptunnel: could not resolve cloud relay %s to pin its route: %v", host, rerr)
		}
		for _, addr := range addrs {
			ips = append(ips, addr.String())
		}
	}

	return ips
}

// ConnectVPNClient opens a VPN data channel on session, sets up this
// machine's local TUN device, does the open/ready handshake with the
// remote end, then routes this machine's traffic through the tunnel (only
// routes, as common.ParseVPNRoutes gives them, if any are given) and
// pumps packets until ctx is done or the channel drops -- undoing every
// change it made before returning. It doesn't own session itself (never
// closes it), since session may be shared with other features.
//
// All privileged networking goes through helper (a deskconn-rootd
// connection, see iptun.LaunchHelper) rather than being done directly, so
// neither the CLI nor deskconn-userd needs any capability grant of its own.
//
// onReady, if non-nil, is called once the remote end confirms the tunnel
// and this machine's traffic has been routed through it -- callers printing
// something like "tunnel up" should wait for this rather than assume
// success as soon as the call is made.
func ConnectVPNClient(ctx context.Context, session *webrtc.WebRTCSession, helper *common.VPNHelperClient,
	routes common.VPNRoutes, onReady func()) error {
	tun, ifaceName, err := helper.OpenTUN(common.VPNClientTUNName)
	if err != nil {
		return fmt.Errorf("failed to create tun device via deskconn-rootd: %w", err)
	}

	var teardown []func()
	runTeardown := func() {
		for i := len(teardown) - 1; i >= 0; i-- {
			teardown[i]()
		}
	}
	defer runTeardown()
	teardown = append(teardown, func() { _ = tun.Close() })

	if err := helper.ConfigureTUNAddress(ifaceName, common.VPNClientCIDR, common.VPNMTU); err != nil {
		return err
	}

	// Sees the DNS answers coming back through the tunnel, to route the addresses of
	// routes.Domains as they're looked up; a no-op without domains.
	router := NewDNSRouter(routes.Domains, func(cidr string) error { return helper.AddTunnelRoute(cidr, ifaceName) })

	ordered, maxRetransmits := false, uint16(common.VPNMaxRetransmits)
	channel, err := session.OpenChannel(common.VPNChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		return err
	}
	teardown = append(teardown, func() { _ = channel.Close() })

	// Registered up front so a rejection on the remote end (e.g. not armed to serve) is
	// noticed the instant the channel closes, instead of sitting out the full
	// VPNHandshakeTimeout waiting for a "ready" frame that was never coming.
	closedCh := make(chan struct{})
	var closedOnce sync.Once
	signalClosed := func() { closedOnce.Do(func() { close(closedCh) }) }
	channel.OnClose(signalClosed)
	channel.OnError(func(error) { signalClosed() })

	openCh := make(chan struct{})
	channel.OnOpen(func() { close(openCh) })
	select {
	case <-openCh:
	case <-closedCh:
		return fmt.Errorf("remote machine closed the vpn channel before it opened")
	case <-time.After(common.VPNHandshakeTimeout):
		return fmt.Errorf("timed out opening vpn data channel")
	case <-ctx.Done():
		return ctx.Err()
	}

	readyCh := make(chan common.VPNReadyFrame, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			var frame common.VPNReadyFrame
			if json.Unmarshal(msg.Data, &frame) == nil && frame.Type == common.VPNFrameReady {
				select {
				case readyCh <- frame:
				default:
				}
			}
			return
		}
		if _, werr := tun.Write(router.Filter(msg.Data)); werr != nil {
			log.Debugf("iptunnel: tun write failed: %v", werr)
		}
	})

	openFrame, err := json.Marshal(common.VPNOpenFrame{Type: common.VPNFrameOpen})
	if err != nil {
		return err
	}
	if err := channel.SendText(string(openFrame)); err != nil {
		return err
	}

	var ready common.VPNReadyFrame
	select {
	case ready = <-readyCh:
	case <-closedCh:
		return fmt.Errorf("remote machine rejected the tunnel (not currently serving? " +
			"it needs \"desk vpn serve\" run on it first)")
	case <-time.After(common.VPNHandshakeTimeout):
		return fmt.Errorf("timed out waiting for the remote machine to set up the tunnel")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Pin the addresses that must keep working after the default route is replaced -- see
	// pinTargets. Each goes via its own family's default route: the WebRTC peer may well be
	// reached over IPv6, which is blocked below.
	for _, peerIP := range pinTargets(ctx, session) {
		ipVersion := 4
		if parsed := net.ParseIP(peerIP); parsed != nil && parsed.To4() == nil {
			ipVersion = 6
		}
		rt, rerr := common.GetDefaultRoute(ipVersion)
		if rerr != nil {
			log.Debugf("iptunnel: no ipv%d default route to pin %s to: %v", ipVersion, peerIP, rerr)
			continue
		}
		if aerr := helper.AddHostRoute(peerIP, rt.Gateway, rt.Iface); aerr == nil {
			ip := peerIP
			teardown = append(teardown, func() { _ = helper.DelHostRoute(ip) })
		} else {
			log.Debugf("iptunnel: could not pin route to %s, tunnel may loop or drop: %v", peerIP, aerr)
		}
	}

	if !routes.Empty() {
		// Only these destinations use the tunnel: IPv6 and other lookups stay as they are, since
		// blocking or taking them over would affect everything else on this machine too.
		for _, route := range routes.CIDRs {
			if err := helper.AddTunnelRoute(route, ifaceName); err != nil {
				return fmt.Errorf("failed to route %s through the tunnel: %w", route, err)
			}
			added := route
			teardown = append(teardown, func() { _ = helper.DelTunnelRoute(added, ifaceName) })
		}
		if len(routes.Domains) > 0 {
			if err := routeDomains(helper, ifaceName, ready.DNS, routes, router, &teardown); err != nil {
				return err
			}
		}
	} else if err := routeAllTraffic(helper, ifaceName, ready.DNS, &teardown); err != nil {
		return err
	}

	common.SafeGo(func() { common.PumpTUNToChannel(tun, channel, closedCh) })

	// Looked up once now, through the tunnel, so their current addresses are routed before
	// anything else on this machine asks for them.
	primeDomains(ctx, routes.Domains)

	if onReady != nil {
		onReady()
	}

	select {
	case <-closedCh:
	case <-ctx.Done():
	}
	signalClosed()

	return nil
}

// routeAllTraffic sends all of this machine's IPv4 traffic and DNS lookups through the tunnel
// on ifaceName and blocks IPv6, appending the undo of each step to teardown.
func routeAllTraffic(helper *common.VPNHelperClient, ifaceName string, exitDNS []string, teardown *[]func()) error {
	if err := helper.ReplaceDefaultRoute(4, ifaceName); err != nil {
		return fmt.Errorf("failed to change default route: %w", err)
	}
	*teardown = append(*teardown, func() { _ = helper.RestoreDefaultRoute(4) })

	// Best-effort: this is only closing an IPv6 leak, not the tunnel itself.
	if verr := helper.BlockIPv6Default(); verr == nil {
		*teardown = append(*teardown, func() { _ = helper.RestoreIPv6Default() })
	} else {
		log.Debugf("iptunnel: could not block ipv6 default route, ipv6 traffic may bypass the tunnel: %v", verr)
	}

	// Lookups would otherwise still go to this network's own resolvers -- leaking every name
	// looked up, and getting answers picked for this location instead of the exit node's.
	dnsServers := common.TunnelDNSServers(exitDNS)
	if len(dnsServers) == 0 {
		dnsServers = common.VPNFallbackDNS()
	}
	if derr := helper.SetLinkDNS(ifaceName, dnsServers); derr == nil {
		*teardown = append(*teardown, func() { _ = helper.RevertLinkDNS(ifaceName) })
	} else {
		log.Warnf("iptunnel: could not route dns through the tunnel, lookups will use this "+
			"network's resolvers: %v", derr)
	}
	return nil
}

// routeDomains sends lookups of routes.Domains (and their subdomains) to the exit node's
// resolvers through the tunnel on ifaceName, where router routes the addresses in the answers.
// Each step's undo is appended to teardown.
func routeDomains(helper *common.VPNHelperClient, ifaceName string, exitDNS []string, routes common.VPNRoutes,
	router *DNSRouter, teardown *[]func()) error {
	servers := common.TunnelDNSServers(exitDNS)
	if len(servers) == 0 {
		servers = common.VPNFallbackDNS()
	}
	for _, server := range servers {
		cidr := server + "/32"
		if slices.Contains(routes.CIDRs, cidr) {
			continue
		}
		if err := helper.AddTunnelRoute(cidr, ifaceName); err != nil {
			return fmt.Errorf("failed to route dns server %s through the tunnel: %w", server, err)
		}
		*teardown = append(*teardown, func() { _ = helper.DelTunnelRoute(cidr, ifaceName) })
	}
	*teardown = append(*teardown, func() {
		for _, cidr := range router.Stop() {
			_ = helper.DelTunnelRoute(cidr, ifaceName)
		}
	})

	allLookups, err := helper.SetLinkDNSDomains(ifaceName, servers, routes.Domains)
	if err != nil {
		return fmt.Errorf("failed to send lookups of %s through the tunnel: %w",
			strings.Join(routes.Domains, ", "), err)
	}
	*teardown = append(*teardown, func() { _ = helper.RevertLinkDNS(ifaceName) })
	if allLookups {
		log.Warnf("iptunnel: systemd-resolved isn't answering this machine's lookups, so all of them go "+
			"through the tunnel; only the addresses of %s are routed through it", strings.Join(routes.Domains, ", "))
	}
	return nil
}

// primeDomains looks each of domains up, for the DNSRouter to route their addresses.
func primeDomains(ctx context.Context, domains []string) {
	for _, domain := range domains {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := net.DefaultResolver.LookupIPAddr(lookupCtx, domain); err != nil {
			log.Debugf("iptunnel: could not look up %s through the tunnel: %v", domain, err)
		}
		cancel()
	}
}
