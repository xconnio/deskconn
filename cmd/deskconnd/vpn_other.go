//go:build !linux

package main

import (
	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

// registerVPNProcedures is a no-op here: VPN serving needs the Linux-only iptun package (see
// iptunnel.go), so on every other platform the vpn.start/vpn.stop RPCs simply aren't
// registered -- same as the D-Bus-only procedures in deskconn.go's Register.
func registerVPNProcedures(*xconn.Session, func() *deskconn.Deskconn) error {
	return nil
}

// dataChannelHandler routes incoming WebRTC data channels straight to file-stream handling,
// since there's no VPN channel classification to do on this platform.
func dataChannelHandler(d *deskconn.Deskconn) func(string, *webrtc.DataChannel, []byte) {
	return d.HandleFileStreamChannel
}

// closeVPNTunnel is a no-op here: there's never an active VPN tunnel to tear down.
func closeVPNTunnel(*deskconn.Deskconn) {}
