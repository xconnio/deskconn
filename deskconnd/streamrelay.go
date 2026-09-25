package deskconnd

import (
	"io"
	"net"

	"github.com/xconnio/deskconn/common"
)

func ReadRelayHeader(r io.Reader) (common.RelayHeader, error) {
	var h common.RelayHeader
	err := common.ReadMsg(r, &h)
	return h, err
}

// ServeStreamRelay accepts xlink's relayed local connections on ln and
// dispatches each to the feature it belongs to.
func (d *Deskconn) ServeStreamRelay(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		common.SafeGo(func() { d.handleRelayConn(conn) })
	}
}

// handleRelayConn reads the RelayHeader xlink wrote and resumes handling
// the connection where xlink's classification left off.
func (d *Deskconn) handleRelayConn(conn net.Conn) {
	header, err := ReadRelayHeader(conn)
	if err != nil {
		conn.Close()
		return
	}

	if header.Kind == common.RelayKindQUIC {
		d.DispatchQUICOp(header.Op, conn)
		return
	}

	// WebRTC-originated. VPN's first message is classification-only and
	// never relayed (see xlink's channel classification); every other
	// feature's first message -- its ephemeral public key -- is real
	// payload, relayed as the first frame.
	if header.Label == common.VPNChannelLabel {
		d.handleVPNChannel(NewRelayChannel(conn))
		return
	}

	firstMessage, _, err := common.ReadRelayFrame(conn)
	if err != nil {
		conn.Close()
		return
	}
	channel := NewRelayChannel(conn)

	switch header.Label {
	case common.ShellChannelLabel:
		d.HandleShellChannel("", channel, firstMessage)
	case common.PortForwardChannelLabel:
		d.HandlePortForwardChannel("", channel, firstMessage)
	case common.PortReverseChannelLabel:
		d.HandlePortReverseChannel("", channel, firstMessage)
	case common.AgentForwardChannelLabel:
		d.HandleAgentForwardChannel("", channel, firstMessage)
	case common.LogChannelLabel:
		d.HandleLogsChannel("", channel, firstMessage)
	default:
		d.HandleFileStreamChannel("", channel, firstMessage)
	}
}
