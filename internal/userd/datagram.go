package userd

import (
	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

// HandleDatagramChannel accepts the VPN's native unreliable channel. Reliable
// feature traffic uses RPC streams, so other labels have no raw-channel path.
func (d *Deskconn) HandleDatagramChannel(channel *xconn.WebRTCDataChannel) {
	if channel.Label() != common.VPNChannelLabel {
		_ = channel.Close()
		return
	}
	common.SafeGo(func() { d.handleVPNChannel(channel) })
}
