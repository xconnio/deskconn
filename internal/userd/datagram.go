package userd

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

type vpnChannelPair struct {
	data    *xconn.WebRTCDataChannel
	control *xconn.WebRTCDataChannel
	expiry  *time.Timer
}

type vpnChannelPairs struct {
	mu      sync.Mutex
	pending map[string]*vpnChannelPair
}

// HandleDatagramChannel pairs VPN channels belonging to the same authenticated
// WebRTC connection.
func (d *Deskconn) HandleDatagramChannel(connectionID string, ch *xconn.WebRTCDataChannel) {
	switch ch.Label() {
	case common.VPNControlChannelLabel, common.VPNDataChannelLabel:
		if connectionID != "" {
			d.pairVPNChannel(connectionID, ch)
			return
		}
	}
	_ = ch.Close()
}

func (d *Deskconn) pairVPNChannel(id string, ch *xconn.WebRTCDataChannel) {
	pairs := &d.vpnChannels
	pairs.mu.Lock()
	pair := pairs.pending[id]
	if pair == nil {
		pair = &vpnChannelPair{}
		if pairs.pending == nil {
			pairs.pending = make(map[string]*vpnChannelPair)
		}
		pairs.pending[id] = pair
		pair.expiry = time.AfterFunc(common.VPNHandshakeTimeout, func() {
			pairs.mu.Lock()
			if pairs.pending[id] != pair {
				pairs.mu.Unlock()
				return
			}
			delete(pairs.pending, id)
			pairs.mu.Unlock()
			_ = ch.Close()
		})
	}

	slot := &pair.data
	if ch.Label() == common.VPNControlChannelLabel {
		slot = &pair.control
	}
	if *slot != nil {
		pairs.mu.Unlock()
		_ = ch.Close()
		return
	}
	*slot = ch
	if pair.data == nil || pair.control == nil {
		pairs.mu.Unlock()
		return
	}
	delete(pairs.pending, id)
	pair.expiry.Stop()
	pairs.mu.Unlock()
	d.serveVPNChannels(pair.data, pair.control)
}

func (d *Deskconn) serveVPNChannels(data, control *xconn.WebRTCDataChannel) {
	defer func() {
		_ = data.Close()
		_ = control.Close()
	}()

	open := make(chan struct{}, 1)
	control.OnMessage(func(msg xconn.WebRTCDataChannelMessage) {
		var frame common.VPNOpenFrame
		if msg.IsString && json.Unmarshal(msg.Data, &frame) == nil && frame.Type == common.VPNFrameOpen {
			select {
			case open <- struct{}{}:
			default:
			}
		}
	})
	select {
	case <-open:
	case <-data.Done():
		return
	case <-control.Done():
		return
	case <-time.After(common.VPNHandshakeTimeout):
		return
	}

	d.handleVPNChannel(data, control)
	select {
	case <-data.Done():
	case <-control.Done():
	}
}
