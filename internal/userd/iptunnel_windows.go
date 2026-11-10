//go:build windows

package userd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
)

// On Windows the exit node is a userspace NAT rather than the Linux TUN + iptables setup: a
// netstack terminates the client's TCP and UDP and redials each flow with ordinary sockets. That
// needs no driver, no elevation and no WinNAT, and changes nothing on the machine. Of ICMP, only
// echo requests are carried, answered with IcmpSendEcho (see icmp_windows.go), which needs no
// elevation either.

const (
	vpnNICID          = 1
	vpnQueuePackets   = 1024
	vpnTCPMaxInFlight = 1024
	vpnDialTimeout    = 15 * time.Second
	vpnUDPIdleTimeout = 60 * time.Second

	// Same bounds as common.PumpTUNToChannel, which only reads from a device.
	vpnSendBufferHigh = 512 * 1024
	vpnSendBufferLow  = 256 * 1024
)

// vpnServer mirrors iptunnel.go's: armed by "desk vpn serve", one tunnel at a time.
type vpnServer struct {
	mu       sync.Mutex
	armed    bool
	starting bool
	closed   bool
	active   *vpnTunnelSession
}

func newVPNServer() *vpnServer {
	return &vpnServer{}
}

type vpnTunnelSession struct {
	channel   common.MessageChannel
	stack     *stack.Stack
	endpoint  *channel.Endpoint
	ctx       context.Context
	cancel    context.CancelFunc
	allowed   func(net.IP) bool
	wg        sync.WaitGroup
	closeOnce sync.Once

	// pinger answers pings (see icmp_windows.go), nil if it couldn't be set up. echoMu keeps
	// a ping from starting once close has begun waiting for them.
	pinger     *icmpPinger
	echoMu     sync.Mutex
	echoClosed bool
}

func (d *Deskconn) handleVPNChannel(ch common.MessageChannel) {
	d.vpn.mu.Lock()
	if d.vpn.closed || !d.vpn.armed {
		d.vpn.mu.Unlock()
		log.Warnln("iptunnel: rejecting VPN channel, this machine isn't currently serving " +
			"(run \"desk vpn serve\" to allow it)")
		_ = ch.Close()
		return
	}
	if d.vpn.active != nil || d.vpn.starting {
		d.vpn.mu.Unlock()
		log.Warnln("iptunnel: rejecting new VPN channel, a tunnel is already active or starting")
		_ = ch.Close()
		return
	}
	d.vpn.starting = true
	d.vpn.mu.Unlock()

	sess, err := startVPNTunnelServer(ch, vpnDestinationAllowed)

	d.vpn.mu.Lock()
	d.vpn.starting = false
	if err != nil || d.vpn.closed || !d.vpn.armed {
		d.vpn.mu.Unlock()
		if err != nil {
			log.Errorf("iptunnel: failed to start tunnel: %v", err)
		} else {
			sess.close()
		}
		_ = ch.Close()
		return
	}
	d.vpn.active = sess
	d.vpn.mu.Unlock()

	ch.OnClose(func() { common.SafeGo(func() { d.closeVPNSession(sess) }) })
	ch.OnError(func(err error) {
		log.Debugf("iptunnel: channel error: %v", err)
		common.SafeGo(func() { d.closeVPNSession(sess) })
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

// CloseVPNTunnel tears down the active tunnel and refuses new ones, for process shutdown.
func (d *Deskconn) CloseVPNTunnel() {
	d.vpn.mu.Lock()
	sess := d.vpn.active
	d.vpn.active = nil
	d.vpn.armed = false
	d.vpn.closed = true
	d.vpn.mu.Unlock()

	if sess != nil {
		sess.close()
	}
}

// startVPNTunnelServer brings up the netstack for one client. allowed decides which destinations
// the client may reach.
func startVPNTunnelServer(ch common.MessageChannel, allowed func(net.IP) bool) (*vpnTunnelSession, error) {
	netstack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	endpoint := channel.New(vpnQueuePackets, common.VPNMTU, "")
	ctx, cancel := context.WithCancel(context.Background())
	sess := &vpnTunnelSession{
		channel:  ch,
		stack:    netstack,
		endpoint: endpoint,
		ctx:      ctx,
		cancel:   cancel,
		allowed:  allowed,
	}

	fail := func(what string, terr tcpip.Error) (*vpnTunnelSession, error) {
		sess.close()
		return nil, fmt.Errorf("%s: %s", what, terr)
	}
	if terr := netstack.CreateNIC(vpnNICID, endpoint); terr != nil {
		return fail("create nic", terr)
	}
	// Every destination is "local" to the stack, so it terminates flows to anywhere.
	if terr := netstack.SetPromiscuousMode(vpnNICID, true); terr != nil {
		return fail("set promiscuous", terr)
	}
	if terr := netstack.SetSpoofing(vpnNICID, true); terr != nil {
		return fail("set spoofing", terr)
	}
	netstack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: vpnNICID}})
	netstack.SetTransportProtocolHandler(tcp.ProtocolNumber,
		tcp.NewForwarder(netstack, 0, vpnTCPMaxInFlight, sess.forwardTCP).HandlePacket)
	netstack.SetTransportProtocolHandler(udp.ProtocolNumber,
		udp.NewForwarder(netstack, sess.forwardUDP).HandlePacket)

	if pinger, err := newICMPPinger(); err == nil {
		sess.pinger = pinger
	} else {
		log.Warnf("iptunnel: ping won't work through this exit node: %v", err)
	}

	expectedSource := net.ParseIP(common.VPNClientIP).To4()
	ch.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString || !ValidateIPv4Source(msg.Data, expectedSource) {
			return
		}
		if sess.handleEcho(msg.Data) {
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(msg.Data)})
		endpoint.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	})

	sess.wg.Add(1)
	common.SafeGo(func() {
		defer sess.wg.Done()
		sess.pumpToChannel()
	})

	// No DNS listed: the client falls back to public resolvers, which work through any exit node.
	ready, err := json.Marshal(common.VPNReadyFrame{
		Type:       common.VPNFrameReady,
		ServerIP:   common.VPNServerIP,
		ClientCIDR: common.VPNClientCIDR,
		MTU:        common.VPNMTU,
	})
	if err == nil {
		err = ch.SendText(string(ready))
	}
	if err != nil {
		sess.close()
		return nil, err
	}
	return sess, nil
}

func (s *vpnTunnelSession) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.channel.Close()
		s.echoMu.Lock()
		s.echoClosed = true
		s.echoMu.Unlock()
		s.wg.Wait()
		if s.pinger != nil {
			s.pinger.close()
		}
		s.stack.Close()
		s.endpoint.Close()
		s.stack.Wait()
	})
}

// pumpToChannel sends the netstack's outgoing packets to the client, as
// common.PumpTUNToChannel does for a device.
func (s *vpnTunnelSession) pumpToChannel() {
	sendReady := make(chan struct{}, 1)
	s.channel.SetBufferedAmountLowThreshold(vpnSendBufferLow)
	s.channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	for {
		pkt := s.endpoint.ReadContext(s.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		data := append([]byte(nil), view.AsSlice()...)
		view.Release()
		pkt.DecRef()

		for s.channel.BufferedAmount()+uint64(len(data)) > vpnSendBufferHigh {
			select {
			case <-sendReady:
			case <-s.ctx.Done():
				return
			}
		}
		if err := s.channel.Send(data); err != nil {
			return
		}
	}
}

// vpnDestinationAllowed refuses what would make the client local to this machine. A packet routed
// through a Linux exit node can't reach 127.0.0.1, but a socket dialed here would, and dialing
// this machine's own addresses would get past its firewall, as local traffic.
func vpnDestinationAllowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return false
	}
	if vpnSubnet.Contains(ip) {
		return false
	}
	local, err := vpnLocalAddrs.contains(ip)
	return err == nil && !local
}

var _, vpnSubnet, _ = net.ParseCIDR(common.VPNSubnetCIDR) //nolint:gochecknoglobals

// vpnLocalAddrs caches this machine's addresses, as listing them is slow on Windows and
// vpnDestinationAllowed runs for every new flow, inline in the UDP forwarder.
var vpnLocalAddrs localAddrCache //nolint:gochecknoglobals

const vpnLocalAddrsTTL = 5 * time.Second

type localAddrCache struct {
	mu      sync.Mutex
	addrs   []net.IP
	err     error
	expires time.Time
}

func (c *localAddrCache) contains(ip net.IP) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := time.Now(); now.After(c.expires) {
		c.expires = now.Add(vpnLocalAddrsTTL)
		c.addrs = c.addrs[:0]
		var addrs []net.Addr
		addrs, c.err = net.InterfaceAddrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				c.addrs = append(c.addrs, ipNet.IP)
			}
		}
	}
	if c.err != nil {
		return false, c.err
	}
	for _, addr := range c.addrs {
		if addr.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

func (s *vpnTunnelSession) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	ip := net.IP(id.LocalAddress.AsSlice())
	if !s.allowed(ip) {
		r.Complete(true)
		return
	}

	dialer := net.Dialer{Timeout: vpnDialTimeout}
	addr := net.JoinHostPort(ip.String(), fmt.Sprint(id.LocalPort))
	remote, err := dialer.DialContext(s.ctx, "tcp4", addr)
	if err != nil {
		r.Complete(true)
		return
	}
	defer func() { _ = remote.Close() }()

	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, ep)
	defer func() { _ = local.Close() }()

	stop := context.AfterFunc(s.ctx, func() {
		_ = local.Close()
		_ = remote.Close()
	})
	defer stop()

	remoteTCP, ok := remote.(*net.TCPConn)
	if !ok {
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(remoteTCP, local)
		_ = remoteTCP.CloseWrite()
	}()
	_, _ = io.Copy(local, remoteTCP)
	_ = local.CloseWrite()
	wg.Wait()
}

func (s *vpnTunnelSession) forwardUDP(r *udp.ForwarderRequest) {
	id := r.ID()
	ip := net.IP(id.LocalAddress.AsSlice())
	if !s.allowed(ip) {
		return
	}

	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		return
	}
	local := gonet.NewUDPConn(&wq, ep)

	addr := net.JoinHostPort(ip.String(), fmt.Sprint(id.LocalPort))
	remote, err := net.Dial("udp4", addr)
	if err != nil {
		_ = local.Close()
		return
	}

	closeBoth := func() {
		_ = local.Close()
		_ = remote.Close()
	}
	stop := context.AfterFunc(s.ctx, closeBoth)

	common.SafeGo(func() {
		defer stop()
		defer closeBoth()
		relayUDP(remote, local)
	})
	common.SafeGo(func() {
		defer closeBoth()
		relayUDP(local, remote)
	})
}

// relayUDP copies datagrams until src errors or stays quiet for vpnUDPIdleTimeout. A flow that
// only goes one way ends on that timeout, and the next datagram starts a new one.
func relayUDP(dst, src net.Conn) {
	buf := make([]byte, common.VPNMTU)
	for {
		_ = src.SetReadDeadline(time.Now().Add(vpnUDPIdleTimeout))
		n, err := src.Read(buf)
		if err != nil {
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				log.Debugf("iptunnel: udp relay ended: %v", err)
			}
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

// ValidateIPv4Source reports whether pkt is an IPv4 packet whose source address is expected, so a
// peer can't inject traffic spoofing an address that isn't theirs.
func ValidateIPv4Source(pkt []byte, expected net.IP) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	return net.IP(pkt[12:16]).Equal(expected)
}

// ProxyVPNStartHandler arms "desk vpn serve". There's no helper to hand over on Windows.
func ProxyVPNStartHandler(d *Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		d.vpn.mu.Lock()
		defer d.vpn.mu.Unlock()
		if d.vpn.closed {
			return xconn.NewInvocationError(common.ErrOperationFailed, "shutting down")
		}
		if d.vpn.armed {
			return xconn.NewInvocationError(common.ErrOperationFailed, "already serving")
		}
		d.vpn.armed = true
		return xconn.NewInvocationResult()
	}
}

// ProxyVPNStopHandler disarms serving, tearing down any active tunnel.
func ProxyVPNStopHandler(d *Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		d.vpn.mu.Lock()
		armed := d.vpn.armed
		sess := d.vpn.active
		d.vpn.armed = false
		d.vpn.active = nil
		d.vpn.mu.Unlock()

		if !armed {
			return xconn.NewInvocationError(common.ErrOperationFailed, "not currently serving")
		}
		if sess != nil {
			sess.close()
		}
		return xconn.NewInvocationResult()
	}
}
