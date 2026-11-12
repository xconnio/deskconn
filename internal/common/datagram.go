package common

import webrtc "github.com/xconnio/xconn/bindings/go/webrtc"

// MessageChannel is the native datagram channel used by VPN tunnels.
type MessageChannel interface {
	Send(data []byte) error
	SendText(s string) error
	OnMessage(f func(msg webrtc.DataChannelMessage))
	OnClose(f func())
	OnError(f func(err error))
	Close() error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(th uint64)
	OnBufferedAmountLow(f func())
}
