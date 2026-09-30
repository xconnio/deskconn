package common

import (
	"net"
	"sync"

	"github.com/pion/webrtc/v4"
)

// RelayChannel implements MessageChannel over a local connection carrying
// RelayFrames, standing in for a *webrtc.DataChannel that another process
// holds (see RelayWebRTCChannel). The backpressure methods are no-ops: that
// process enforces it, and Send/SendText block on conn's own write buffer.
type RelayChannel struct {
	conn net.Conn

	writeMu sync.Mutex

	handlerMu sync.Mutex
	onMessage func(msg webrtc.DataChannelMessage)
	onClose   func()
	onError   func(err error)

	startOnce sync.Once
	closeOnce sync.Once
}

func NewRelayChannel(conn net.Conn) *RelayChannel {
	return &RelayChannel{conn: conn}
}

func (c *RelayChannel) readLoop() {
	for {
		data, isText, err := ReadRelayFrame(c.conn)
		if err != nil {
			c.handlerMu.Lock()
			onError := c.onError
			c.handlerMu.Unlock()
			if onError != nil {
				onError(err)
			}
			c.signalClosed()
			return
		}
		c.handlerMu.Lock()
		onMessage := c.onMessage
		c.handlerMu.Unlock()
		if onMessage != nil {
			onMessage(webrtc.DataChannelMessage{Data: data, IsString: isText})
		}
	}
}

func (c *RelayChannel) signalClosed() {
	c.closeOnce.Do(func() {
		c.handlerMu.Lock()
		onClose := c.onClose
		c.handlerMu.Unlock()
		if onClose != nil {
			onClose()
		}
	})
}

func (c *RelayChannel) Send(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteRelayFrame(c.conn, data, false)
}

func (c *RelayChannel) SendText(s string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteRelayFrame(c.conn, []byte(s), true)
}

// OnMessage registers the handler and, the first time it's called, starts
// reading -- deferred until here so no frame can be delivered before a
// handler is registered.
func (c *RelayChannel) OnMessage(f func(msg webrtc.DataChannelMessage)) {
	c.handlerMu.Lock()
	c.onMessage = f
	c.handlerMu.Unlock()
	c.startOnce.Do(func() { SafeGo(c.readLoop) })
}

func (c *RelayChannel) OnClose(f func()) {
	c.handlerMu.Lock()
	c.onClose = f
	c.handlerMu.Unlock()
}

func (c *RelayChannel) OnError(f func(error)) {
	c.handlerMu.Lock()
	c.onError = f
	c.handlerMu.Unlock()
}

func (c *RelayChannel) Close() error {
	c.signalClosed()
	return c.conn.Close()
}

func (c *RelayChannel) BufferedAmount() uint64                 { return 0 }
func (c *RelayChannel) SetBufferedAmountLowThreshold(_ uint64) {}
func (c *RelayChannel) OnBufferedAmountLow(_ func())           {}
