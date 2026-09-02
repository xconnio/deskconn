//go:build !linux

package deskconn

import "github.com/pion/webrtc/v4"

// vpnServer is a stub on non-Linux platforms: the VPN/iptunnel feature depends on TUN devices
// and iptables via the iptun package, which is Linux-only (see iptunnel.go, iptun/). Only the
// type needs to exist here, for the Deskconn struct's "vpn" field to compile - nothing outside
// iptunnel.go ever reads it on this platform.
type vpnServer struct{}

func newVPNServer() *vpnServer { return &vpnServer{} }

// HandleAuxDataChannel is the non-Linux counterpart to iptunnel.go's HandleAuxDataChannel:
// there's no VPN channel classification to do here (see vpnServer above), so every data
// channel goes straight to file-stream handling. Production wiring (cmd/deskconnd/vpn_other.go)
// already calls HandleFileStreamChannel directly instead; this exists so that code shared with
// Linux - like filetransferp2p_internal_test.go - can call HandleAuxDataChannel unconditionally.
func (d *Deskconn) HandleAuxDataChannel(sessionID string, channel *webrtc.DataChannel, firstMessage []byte) {
	d.HandleFileStreamChannel(sessionID, channel, firstMessage)
}
