package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/iptun"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

// pinTargets returns the IPv4 addresses that must be pinned to the current
// default route before it's replaced: the WebRTC peer (so the tunnel
// doesn't route into itself) and the cloud relay (so xlink's own
// control connection survives the switch).
func pinTargets(ctx context.Context, session *xconnwebrtc.WebRTCSession) []string {
	var ips []string

	if pair, err := session.Connection().SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil &&
		pair != nil && pair.Remote != nil {
		ips = append(ips, pair.Remote.Address)
	}

	if host, _, err := net.SplitHostPort(common.CloudQUICAddress()); err == nil {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, rerr := net.DefaultResolver.LookupIP(resolveCtx, "ip4", host)
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
// remote end, then routes this machine's traffic through the tunnel and
// pumps packets until ctx is done or the channel drops -- undoing every
// change it made before returning. It doesn't own session itself (never
// closes it), since session may be shared with other features.
//
// All privileged networking goes through helper (a deskconn-vpnd
// connection, see iptun.LaunchHelper) rather than being done directly, so
// neither the CLI nor xlink needs any capability grant of its own.
//
// onReady, if non-nil, is called once the remote end confirms the tunnel
// is actually up -- callers printing something like "tunnel up" should
// wait for this rather than assume success as soon as the call is made.
func ConnectVPNClient(ctx context.Context, session *xconnwebrtc.WebRTCSession, helper *iptun.Client,
	onReady func()) error {
	tun, ifaceName, err := helper.OpenTUN(common.VPNClientTUNName)
	if err != nil {
		return fmt.Errorf("failed to create tun device via deskconn-vpnd: %w", err)
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
		return fmt.Errorf("remote device closed the vpn channel before it opened")
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
		if _, werr := tun.Write(msg.Data); werr != nil {
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

	select {
	case <-readyCh:
	case <-closedCh:
		return fmt.Errorf("remote device rejected the tunnel (not currently serving? " +
			"it needs \"deskconn vpn start\" run on it first)")
	case <-time.After(common.VPNHandshakeTimeout):
		return fmt.Errorf("timed out waiting for the remote device to set up the tunnel")
	case <-ctx.Done():
		return ctx.Err()
	}

	if onReady != nil {
		onReady()
	}

	// Pin the addresses that must keep working after the default route is replaced -- see
	// pinTargets.
	pinPeers := pinTargets(ctx, session)
	if rt, rerr := iptun.GetDefaultRoute(4); rerr == nil {
		for _, peerIP := range pinPeers {
			if aerr := helper.AddHostRoute(peerIP, rt.Gateway, rt.Iface); aerr == nil {
				ip := peerIP
				teardown = append(teardown, func() { _ = helper.DelHostRoute(ip) })
			} else {
				log.Debugf("iptunnel: could not pin route to %s, tunnel may loop or drop: %v", peerIP, aerr)
			}
		}
	}

	prevDefault, err := helper.ReplaceDefaultRoute(4, ifaceName)
	if err != nil {
		return fmt.Errorf("failed to change default route: %w", err)
	}
	teardown = append(teardown, func() { _ = helper.RestoreDefaultRoute(4, prevDefault) })

	// Best-effort: this is only closing an IPv6 leak, not the tunnel itself.
	if hadV6, prevV6, verr := helper.BlockIPv6Default(); verr == nil {
		teardown = append(teardown, func() { _ = helper.RestoreIPv6Default(hadV6, prevV6) })
	} else {
		log.Debugf("iptunnel: could not block ipv6 default route, ipv6 traffic may bypass the tunnel: %v", verr)
	}

	common.SafeGo(func() { common.PumpTUNToChannel(tun, channel, closedCh) })

	select {
	case <-closedCh:
	case <-ctx.Done():
	}
	signalClosed()

	return nil
}
