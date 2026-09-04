//go:build linux

package main

import (
	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

// registerVPNProcedures registers the vpn start/stop RPCs -- VPN serving needs the Linux-only
// iptun package (see iptunnel.go), so this is Linux-only too; see vpn_other.go for every other
// platform.
func registerVPNProcedures(sess *xconn.Session, getCurrentDeskconn func() *deskconn.Deskconn) error {
	if resp := sess.Register(deskconn.ProcedureProxyVPNStart,
		deskconn.ProxyVPNStartHandler(getCurrentDeskconn)).Do(); resp.Err != nil {
		return resp.Err
	}
	if resp := sess.Register(deskconn.ProcedureProxyVPNStop,
		deskconn.ProxyVPNStopHandler(getCurrentDeskconn)).Do(); resp.Err != nil {
		return resp.Err
	}
	return nil
}

// dataChannelHandler routes incoming WebRTC data channels: on Linux this also classifies VPN
// channels (see iptunnel.go's HandleAuxDataChannel) before falling through to file-stream
// handling for everything else.
func dataChannelHandler(d *deskconn.Deskconn) func(string, *webrtc.DataChannel, []byte) {
	return d.HandleAuxDataChannel
}

// closeVPNTunnel tears down any active VPN tunnel and refuses new ones, on daemon
// shutdown/detach.
func closeVPNTunnel(d *deskconn.Deskconn) {
	d.CloseVPNTunnel()
}
