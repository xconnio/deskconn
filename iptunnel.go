package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/iptun"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	VPNServerTUNName = "dtun0"
	VPNClientTUNName = "dtun1"

	VPNServerIP   = "10.66.0.1"
	VPNClientIP   = "10.66.0.2"
	VPNServerCIDR = VPNServerIP + "/24"
	VPNClientCIDR = VPNClientIP + "/24"
	VPNSubnetCIDR = "10.66.0.0/24"

	// VPNMTU keeps every tunneled packet inside one SCTP/DTLS/UDP datagram,
	// so one TUN read maps to exactly one DataChannel message and neither
	// side needs a reassembly protocol of its own.
	VPNMTU = 1200

	VPNChannelLabel = "vpn"

	// VPNFrameOpen  and VPNFrameReady are the "type" discriminators for the two
	// control frames exchanged -- as DataChannel *text* messages -- before
	// either side starts treating channel messages as raw binary IP
	// packets. VPNFrameOpen also lets the server's single OnDataChannel
	// callback tell a VPN channel apart from a file-stream channel; see
	// HandleAuxDataChannel.
	VPNFrameOpen  = "vpn-open"
	VPNFrameReady = "vpn-ready"

	// vpnHandshakeTimeout bounds how long the client waits for the channel
	// to open and for the server's ready frame.
	vpnHandshakeTimeout = 15 * time.Second

	vpnSendBufferHigh = 512 * 1024
	vpnSendBufferLow  = 256 * 1024
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
}

// vpnServer tracks whether this Deskconn is currently willing to act as a
// VPN exit node (helper set by ArmVPNServing -- see "deskconn vpn start"),
// and the single tunnel it allows at a time once it is.
type vpnServer struct {
	mu     sync.Mutex
	helper *iptun.Client // non-nil while armed by ArmVPNServing
	active *vpnTunnelSession

	// starting is claimed before setup begins so two tunnels can't race to
	// use the same helper connection concurrently -- active alone isn't
	// enough, since it's only set once setup finishes.
	starting bool

	closed bool // set by CloseVPNTunnel; refuses new tunnels once shutdown has begun
}

func newVPNServer() *vpnServer {
	return &vpnServer{}
}

type vpnTunnelSession struct {
	tun       *os.File
	channel   *webrtc.DataChannel
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	cleanup   func()
}

// HandleAuxDataChannel is the single callback wired to the WebRTC
// provider's OnDataChannel; it classifies each channel by a "type" field
// in its first message and dispatches to the matching handler.
func (d *Deskconn) HandleAuxDataChannel(sessionID string, channel *webrtc.DataChannel, firstMessage []byte) {
	var probe VPNOpenFrame
	if json.Unmarshal(firstMessage, &probe) == nil && probe.Type == VPNFrameOpen {
		d.handleVPNChannel(channel)
		return
	}

	d.HandleFileStreamChannel(sessionID, channel, firstMessage)
}

func (d *Deskconn) handleVPNChannel(channel *webrtc.DataChannel) {
	d.vpn.mu.Lock()
	if d.vpn.closed || d.vpn.helper == nil {
		d.vpn.mu.Unlock()
		log.Warnln("iptunnel: rejecting VPN channel, this machine isn't currently serving " +
			"(run \"deskconn vpn start\" to allow it)")
		_ = channel.Close()
		return
	}
	if d.vpn.active != nil || d.vpn.starting {
		d.vpn.mu.Unlock()
		log.Warnln("iptunnel: rejecting new VPN channel, a tunnel is already active or starting")
		_ = channel.Close()
		return
	}
	d.vpn.starting = true // claim the setup slot before releasing the lock -- see vpnServer.starting
	helper := d.vpn.helper
	d.vpn.mu.Unlock()

	sess, err := startVPNTunnelServer(channel, helper)

	d.vpn.mu.Lock()
	d.vpn.starting = false
	if err != nil || d.vpn.closed || d.vpn.helper == nil || d.vpn.active != nil {
		// Setup failed, or we were stopped/raced while setting up: don't leak this session's
		// NAT/forwarding state.
		d.vpn.mu.Unlock()
		if err != nil {
			log.Errorf("iptunnel: failed to start tunnel: %v", err)
		} else if sess != nil {
			sess.close()
		}
		_ = channel.Close()
		return
	}
	d.vpn.active = sess
	d.vpn.mu.Unlock()

	// closeVPNSession blocks (waits for the pump goroutine, then execs several iptables/sysctl
	// commands); SafeGo keeps that off the WebRTC library's callback goroutine, since blocking
	// there can stall unrelated traffic on the same connection.
	channel.OnClose(func() { SafeGo(func() { d.closeVPNSession(sess) }) })
	channel.OnError(func(err error) {
		log.Debugf("iptunnel: channel error: %v", err)
		SafeGo(func() { d.closeVPNSession(sess) })
	})
}

func (d *Deskconn) closeVPNSession(sess *vpnTunnelSession) {
	sess.close()
	d.vpn.mu.Lock()
	if d.vpn.active == sess {
		d.vpn.active = nil
	}
	d.vpn.mu.Unlock()
}

// CloseVPNTunnel tears down the active VPN tunnel, disarms serving, and
// refuses any new tunnel or serve session afterward. Call on daemon
// shutdown/detach so a lost connection never leaves networking changed.
func (d *Deskconn) CloseVPNTunnel() {
	d.vpn.mu.Lock()
	sess := d.vpn.active
	helper := d.vpn.helper
	d.vpn.active = nil
	d.vpn.helper = nil
	d.vpn.closed = true
	d.vpn.mu.Unlock()

	if sess != nil {
		sess.close()
	}
	if helper != nil {
		_ = helper.Close()
	}
}

// ArmVPNServing arms d to accept inbound VPN tunnel requests using helper,
// then returns immediately (doesn't block for the serving session's
// duration, so "deskconn vpn start" can return control to its caller right
// away). Stays armed, serving tunnels one at a time, until
// DisarmVPNServing or CloseVPNTunnel.
func (d *Deskconn) ArmVPNServing(helper *iptun.Client) error {
	d.vpn.mu.Lock()
	defer d.vpn.mu.Unlock()

	if d.vpn.closed || d.vpn.helper != nil {
		return fmt.Errorf("already serving, or shutting down")
	}
	d.vpn.helper = helper
	return nil
}

// DisarmVPNServing stops accepting new inbound VPN tunnels, tears down
// whichever one is currently active, if any, and closes the helper
// connection -- causing deskconn-vpnd to unwind and exit. Reports whether
// serving was actually armed.
func (d *Deskconn) DisarmVPNServing() bool {
	d.vpn.mu.Lock()
	helper := d.vpn.helper
	sess := d.vpn.active
	d.vpn.helper = nil
	d.vpn.active = nil
	d.vpn.mu.Unlock()

	if helper == nil {
		return false
	}
	if sess != nil {
		sess.close()
	}
	_ = helper.Close()
	return true
}

func startVPNTunnelServer(channel *webrtc.DataChannel, helper *iptun.Client) (*vpnTunnelSession, error) {
	tun, ifaceName, err := helper.OpenTUN(VPNServerTUNName)
	if err != nil {
		return nil, fmt.Errorf("open tun: %w", err)
	}

	var cleanupFns []func()
	rollback := func() {
		for i := len(cleanupFns) - 1; i >= 0; i-- {
			cleanupFns[i]()
		}
		_ = tun.Close()
	}

	if err := helper.ConfigureTUNAddress(ifaceName, VPNServerCIDR, VPNMTU); err != nil {
		rollback()
		return nil, err
	}

	prevForward, err := helper.SetSysctl("net.ipv4.ip_forward", "1")
	if err != nil {
		rollback()
		return nil, fmt.Errorf("enable ip forwarding: %w", err)
	}
	cleanupFns = append(cleanupFns, func() {
		if rerr := helper.RestoreSysctl("net.ipv4.ip_forward", prevForward); rerr != nil {
			log.Debugf("iptunnel: failed to restore ip_forward: %v", rerr)
		}
	})

	egress, err := iptun.GetDefaultRoute(4)
	if err != nil {
		rollback()
		return nil, fmt.Errorf("determine internet-facing interface: %w", err)
	}

	if err := helper.AddMasquerade(VPNSubnetCIDR, egress.Iface); err != nil {
		rollback()
		return nil, err
	}
	cleanupFns = append(cleanupFns, func() {
		if derr := helper.DelMasquerade(VPNSubnetCIDR, egress.Iface); derr != nil {
			log.Debugf("iptunnel: failed to remove masquerade rule: %v", derr)
		}
	})

	if err := helper.AddForwardAccept(ifaceName, egress.Iface); err != nil {
		rollback()
		return nil, err
	}
	cleanupFns = append(cleanupFns, func() {
		if derr := helper.DelForwardAccept(ifaceName, egress.Iface); derr != nil {
			log.Debugf("iptunnel: failed to remove forward rule: %v", derr)
		}
	})

	if err := helper.AddForwardEstablished(egress.Iface, ifaceName); err != nil {
		rollback()
		return nil, err
	}
	cleanupFns = append(cleanupFns, func() {
		if derr := helper.DelForwardEstablished(egress.Iface, ifaceName); derr != nil {
			log.Debugf("iptunnel: failed to remove forward rule: %v", derr)
		}
	})

	sess := &vpnTunnelSession{
		tun:     tun,
		channel: channel,
		done:    make(chan struct{}),
		cleanup: func() {
			for i := len(cleanupFns) - 1; i >= 0; i-- {
				cleanupFns[i]()
			}
		},
	}

	expectedSource := net.ParseIP(VPNClientIP).To4()
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			return // control frame; nothing more expected once we're running
		}
		if !ValidateIPv4Source(msg.Data, expectedSource) {
			return
		}
		if _, werr := tun.Write(msg.Data); werr != nil {
			log.Debugf("iptunnel: tun write failed: %v", werr)
		}
	})

	sess.wg.Add(1)
	SafeGo(func() {
		defer sess.wg.Done()
		PumpTUNToChannel(tun, channel, sess.done)
	})

	ready, err := json.Marshal(VPNReadyFrame{
		Type:       VPNFrameReady,
		ServerIP:   VPNServerIP,
		ClientCIDR: VPNClientCIDR,
		MTU:        VPNMTU,
	})
	if err != nil {
		sess.close()
		return nil, err
	}
	if err := channel.SendText(string(ready)); err != nil {
		sess.close()
		return nil, err
	}

	return sess, nil
}

func (s *vpnTunnelSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.tun.Close()
		_ = s.channel.Close()
		s.wg.Wait()
		if s.cleanup != nil {
			s.cleanup()
		}
	})
}

// pinTargets returns the IPv4 addresses that must be pinned to the current
// default route before it's replaced: the WebRTC peer (so the tunnel
// doesn't route into itself) and the cloud relay (so deskconnd's own
// control connection survives the switch).
func pinTargets(ctx context.Context, session *xconnwebrtc.WebRTCSession) []string {
	var ips []string

	if pair, err := session.Connection().SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil &&
		pair != nil && pair.Remote != nil {
		ips = append(ips, pair.Remote.Address)
	}

	if host, _, err := net.SplitHostPort(CloudQUICAddress()); err == nil {
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
// neither the CLI nor deskconnd needs any capability grant of its own.
//
// onReady, if non-nil, is called once the remote end confirms the tunnel
// is actually up -- callers printing something like "tunnel up" should
// wait for this rather than assume success as soon as the call is made.
func ConnectVPNClient(ctx context.Context, session *xconnwebrtc.WebRTCSession, helper *iptun.Client,
	onReady func()) error {
	tun, ifaceName, err := helper.OpenTUN(VPNClientTUNName)
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

	if err := helper.ConfigureTUNAddress(ifaceName, VPNClientCIDR, VPNMTU); err != nil {
		return err
	}

	ordered, maxRetransmits := false, uint16(0)
	channel, err := session.OpenChannel(VPNChannelLabel, &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		return err
	}
	teardown = append(teardown, func() { _ = channel.Close() })

	// Registered up front so a rejection on the remote end (e.g. not armed to serve) is
	// noticed the instant the channel closes, instead of sitting out the full
	// vpnHandshakeTimeout waiting for a "ready" frame that was never coming.
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
	case <-time.After(vpnHandshakeTimeout):
		return fmt.Errorf("timed out opening vpn data channel")
	case <-ctx.Done():
		return ctx.Err()
	}

	readyCh := make(chan VPNReadyFrame, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var frame VPNReadyFrame
		if json.Unmarshal(msg.Data, &frame) == nil && frame.Type == VPNFrameReady {
			select {
			case readyCh <- frame:
			default:
			}
		}
	})

	openFrame, err := json.Marshal(VPNOpenFrame{Type: VPNFrameOpen})
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
	case <-time.After(vpnHandshakeTimeout):
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

	// closedCh (registered above) covers this phase too, so no need to re-register.
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			return
		}
		_, _ = tun.Write(msg.Data)
	})

	SafeGo(func() { PumpTUNToChannel(tun, channel, closedCh) })

	select {
	case <-closedCh:
	case <-ctx.Done():
	}
	signalClosed()

	return nil
}

// PumpTUNToChannel reads raw IP packets from tun and sends each as one
// binary DataChannel message, blocking on the channel's own backpressure
// rather than buffering unboundedly. Shared by both server and client.
func PumpTUNToChannel(tun *os.File, channel *webrtc.DataChannel, done <-chan struct{}) {
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

// ValidateIPv4Source reports whether pkt is an IPv4 packet whose source
// address is expected. Used so a peer can't inject traffic spoofing an
// address that isn't theirs.
func ValidateIPv4Source(pkt []byte, expected net.IP) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	return net.IP(pkt[12:16]).Equal(expected)
}
