//go:build windows

package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/vpnwin"
	"github.com/xconnio/xconn/bindings/go/webrtc"
)

// The same bounds as common.PumpTUNToChannel, which only reads from a file.
const (
	vpnSendBufferHigh = 512 * 1024
	vpnSendBufferLow  = 256 * 1024
)

// pinTargets returns what must keep using the current default route once it's replaced: the
// WebRTC peer, so the tunnel doesn't route into itself, and the cloud relay.
func pinTargets(ctx context.Context, session *webrtc.WebRTCSession) []net.IP {
	var ips []net.IP

	if address, err := session.Connection().SelectedRemoteAddress(ctx); err == nil {
		if ip := net.ParseIP(address); ip != nil {
			ips = append(ips, ip)
		}
	}

	if host, _, err := net.SplitHostPort(common.CloudQUICAddress()); err == nil {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, rerr := net.DefaultResolver.LookupIP(resolveCtx, "ip", host)
		cancel()
		if rerr != nil {
			log.Debugf("iptunnel: could not resolve cloud relay %s to pin its route: %v", host, rerr)
		}
		ips = append(ips, addrs...)
	}
	return ips
}

// ConnectVPNClient does what the Linux one does, in this process rather than through a root
// helper: the Windows tunnel process is itself elevated, as Wintun sessions can't leave the
// process that started them. Removing the device on the way out undoes everything it set up.
func ConnectVPNClient(ctx context.Context, session *webrtc.WebRTCSession, onReady func()) error {
	device, err := vpnwin.Open(common.VPNClientTUNName)
	if err != nil {
		return err
	}
	defer device.Close()

	if err := device.Configure(common.VPNClientCIDR, common.VPNMTU); err != nil {
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
	defer func() { _ = channel.Close() }()

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
		return errors.New("remote machine closed the vpn channel before it opened")
	case <-time.After(common.VPNHandshakeTimeout):
		return errors.New("timed out opening vpn data channel")
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
		if werr := device.Write(msg.Data); werr != nil {
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
		return errors.New("remote machine rejected the tunnel (not currently serving? " +
			"it needs \"desk vpn serve\" run on it first)")
	case <-time.After(common.VPNHandshakeTimeout):
		return errors.New("timed out waiting for the remote machine to set up the tunnel")
	case <-ctx.Done():
		return ctx.Err()
	}

	for _, ip := range pinTargets(ctx, session) {
		if perr := device.PinHostRoute(ip); perr != nil {
			log.Debugf("iptunnel: could not pin route to %s, tunnel may loop or drop: %v", ip, perr)
		}
	}

	// The pump starts before the routes do, so nothing routed in sits unread.
	var pumpWG sync.WaitGroup
	pumpWG.Add(1)
	common.SafeGo(func() {
		defer pumpWG.Done()
		pumpDeviceToChannel(device, channel, closedCh)
	})
	// Closed before waiting, as Read only returns once the device is gone.
	defer pumpWG.Wait()
	defer device.Close()
	defer signalClosed()

	if err := device.RouteAll(4); err != nil {
		return fmt.Errorf("failed to change default route: %w", err)
	}
	// Best-effort: this is only closing an IPv6 leak, not the tunnel itself.
	if verr := device.RouteAll(6); verr != nil {
		log.Debugf("iptunnel: could not block ipv6, ipv6 traffic may bypass the tunnel: %v", verr)
	}

	dnsServers := common.TunnelDNSServers(ready.DNS)
	if len(dnsServers) == 0 {
		dnsServers = common.VPNFallbackDNS()
	}
	if derr := device.SetDNS(dnsServers); derr != nil {
		log.Warnf("iptunnel: could not route dns through the tunnel, lookups will use this "+
			"network's resolvers: %v", derr)
	}

	if onReady != nil {
		onReady()
	}

	select {
	case <-closedCh:
	case <-ctx.Done():
	}
	return nil
}

func pumpDeviceToChannel(device *vpnwin.Device, channel common.MessageChannel, done <-chan struct{}) {
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
		n, err := device.Read(buf)
		if err != nil {
			return
		}
		for channel.BufferedAmount()+uint64(n) > vpnSendBufferHigh { //nolint:gosec // n isn't negative
			select {
			case <-sendReady:
			case <-done:
				return
			}
		}
		if err := channel.Send(append([]byte(nil), buf[:n]...)); err != nil {
			return
		}
	}
}
