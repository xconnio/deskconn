package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go/webrtc"
)

func openVPNChannels(ctx context.Context, session *webrtc.WebRTCSession, receive func([]byte)) (
	common.MessageChannel, common.VPNReadyFrame, <-chan struct{}, func(), error,
) {
	setupCtx, cancel := context.WithTimeout(ctx, common.VPNHandshakeTimeout)
	defer cancel()

	var ready common.VPNReadyFrame
	control, err := session.Connection().OpenChannelContext(setupCtx, common.VPNControlChannelLabel, nil)
	if err != nil {
		return nil, ready, nil, nil, err
	}

	closed := make(chan struct{})
	var once sync.Once
	onClose := func() { once.Do(func() { close(closed) }) }
	control.OnClose(onClose)
	control.OnError(func(error) { onClose() })
	readyCh := make(chan common.VPNReadyFrame, 1)
	control.OnMessage(func(msg webrtc.DataChannelMessage) {
		var frame common.VPNReadyFrame
		if msg.IsString && json.Unmarshal(msg.Data, &frame) == nil && frame.Type == common.VPNFrameReady {
			select {
			case readyCh <- frame:
			default:
			}
		}
	})

	ordered, retries := false, uint16(0)
	data, err := session.Connection().OpenChannelContext(setupCtx, common.VPNDataChannelLabel,
		&webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retries})
	if err != nil {
		_ = control.Close()
		return nil, ready, nil, nil, err
	}
	closeChannels := func() {
		onClose()
		_ = data.Close()
		_ = control.Close()
	}
	data.OnClose(onClose)
	data.OnError(func(error) { onClose() })
	data.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			receive(msg.Data)
		}
	})

	open, err := json.Marshal(common.VPNOpenFrame{Type: common.VPNFrameOpen})
	if err == nil {
		err = control.SendText(string(open))
	}
	if err != nil {
		closeChannels()
		return nil, ready, nil, nil, err
	}
	select {
	case ready = <-readyCh:
		return data, ready, closed, closeChannels, nil
	case <-closed:
		err = errors.New("remote machine closed the VPN channels during setup")
	case <-setupCtx.Done():
		err = setupCtx.Err()
	}
	closeChannels()
	return nil, ready, nil, nil, err
}
