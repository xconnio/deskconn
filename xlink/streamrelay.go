package xlink

import (
	"io"
	"net"
	"sync"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn/common"
)

func WriteRelayHeader(w io.Writer, h common.RelayHeader) error {
	return common.WriteMsg(w, h)
}

// RelayWebRTCChannel splices channel's messages to/from conn as RelayFrames
// in both directions, respecting the real channel's send backpressure. If
// firstMessage is non-nil, it's relayed as the first frame.
func RelayWebRTCChannel(channel common.MessageChannel, conn net.Conn, firstMessage []byte) {
	defer conn.Close()

	if firstMessage != nil {
		if err := common.WriteRelayFrame(conn, firstMessage, true); err != nil {
			_ = channel.Close()
			return
		}
	}

	closed := make(chan struct{})
	var closeOnce sync.Once
	signalClosed := func() { closeOnce.Do(func() { close(closed) }) }
	channel.OnClose(signalClosed)
	channel.OnError(func(error) { signalClosed() })

	msgCh := make(chan webrtc.DataChannelMessage, 32)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case msgCh <- msg:
		case <-closed:
		}
	})

	// channel -> conn
	common.SafeGo(func() {
		for {
			select {
			case msg := <-msgCh:
				if err := common.WriteRelayFrame(conn, msg.Data, msg.IsString); err != nil {
					_ = channel.Close()
					return
				}
			case <-closed:
				return
			}
		}
	})

	// conn -> channel, pausing whenever the real channel's own send buffer is
	// already full rather than queuing unboundedly on top of it.
	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(common.RelayBufferedLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	for {
		data, isText, err := common.ReadRelayFrame(conn)
		if err != nil {
			_ = channel.Close()
			return
		}

		for channel.BufferedAmount()+uint64(len(data)) > common.RelayBufferedHigh {
			select {
			case <-sendReady:
			case <-closed:
				return
			}
		}

		if isText {
			err = channel.SendText(string(data))
		} else {
			err = channel.Send(data)
		}
		if err != nil {
			return
		}
	}
}
