package deskconnd

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/iptun"
)

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
	channel   common.MessageChannel
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	cleanup   func()
}

// handleVPNChannel is called once xlink's channel classification has
// identified channel as a VPN tunnel request (see RelayHeader).
func (d *Deskconn) handleVPNChannel(channel common.MessageChannel) {
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
	channel.OnClose(func() { common.SafeGo(func() { d.closeVPNSession(sess) }) })
	channel.OnError(func(err error) {
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

// CloseVPNTunnel tears down the active VPN tunnel, disarms serving, and
// refuses any new tunnel or serve session afterward. Call on process
// shutdown so a lost connection never leaves networking changed.
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

func startVPNTunnelServer(channel common.MessageChannel, helper *iptun.Client) (*vpnTunnelSession, error) {
	tun, ifaceName, err := helper.OpenTUN(common.VPNServerTUNName)
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

	if err := helper.ConfigureTUNAddress(ifaceName, common.VPNServerCIDR, common.VPNMTU); err != nil {
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

	if err := helper.AddMasquerade(common.VPNSubnetCIDR, egress.Iface); err != nil {
		rollback()
		return nil, err
	}
	cleanupFns = append(cleanupFns, func() {
		if derr := helper.DelMasquerade(common.VPNSubnetCIDR, egress.Iface); derr != nil {
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

	expectedSource := net.ParseIP(common.VPNClientIP).To4()
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
	common.SafeGo(func() {
		defer sess.wg.Done()
		common.PumpTUNToChannel(tun, channel, sess.done)
	})

	ready, err := json.Marshal(common.VPNReadyFrame{
		Type:       common.VPNFrameReady,
		ServerIP:   common.VPNServerIP,
		ClientCIDR: common.VPNClientCIDR,
		MTU:        common.VPNMTU,
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

// ValidateIPv4Source reports whether pkt is an IPv4 packet whose source
// address is expected. Used so a peer can't inject traffic spoofing an
// address that isn't theirs.
func ValidateIPv4Source(pkt []byte, expected net.IP) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	return net.IP(pkt[12:16]).Equal(expected)
}
