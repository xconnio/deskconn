//go:build !linux

package deskconn

import (
	"context"

	"github.com/xconnio/xconn-go"
)

// vpnServer is a stub on non-Linux platforms: the VPN/iptunnel feature depends on TUN devices
// and iptables via the iptun package, which is Linux-only (see iptunnel.go, iptun/). Only the
// type needs to exist here, for the Deskconn struct's "vpn" field to compile - nothing outside
// iptunnel.go ever reads it on this platform.
type vpnServer struct{}

func newVPNServer() *vpnServer { return &vpnServer{} }

// VPNChannelLabel mirrors iptunnel.go's constant of the same name -- streamrelay.go checks
// against it unconditionally (VPN classification happens before the raw-stream feature switch,
// since a VPN channel's first message is classification-only and never relayed as payload), so
// it needs to exist on every platform even though nothing can ever match it here (VPN serving
// never arms, see ProxyVPNStartHandler below).
const VPNChannelLabel = "vpn"

// handleVPNChannel mirrors iptunnel.go's method of the same name: streamrelay.go calls this
// unconditionally on a VPNChannelLabel match, which never happens on this platform, but the
// method still needs to exist to compile. Never actually invoked.
func (d *Deskconn) handleVPNChannel(channel MessageChannel) {
	_ = channel.Close()
}

// CloseVPNTunnel is a no-op here: there's never an active VPN tunnel to tear down on a
// platform that can't serve one in the first place.
func (d *Deskconn) CloseVPNTunnel() {}

// ProxyVPNStartHandler and ProxyVPNStopHandler are stubs on non-Linux platforms: VPN serving
// needs the Linux-only iptun package (see iptunnel.go), so "deskconn vpn start/stop" just
// reports the feature as unavailable here instead of registering nothing (registerLocalProcedures
// wires these unconditionally, so the RPCs must exist on every platform, even if only to fail).
func ProxyVPNStartHandler(*Deskconn) xconn.InvocationHandler {
	return func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
		return xconn.NewInvocationError(ErrOperationFailed, "VPN serving is not supported on this platform")
	}
}

func ProxyVPNStopHandler(*Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		return xconn.NewInvocationError(ErrOperationFailed, "VPN serving is not supported on this platform")
	}
}
