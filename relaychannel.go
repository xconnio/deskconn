package deskconn

import (
	"net"
	"sync"

	"github.com/pion/webrtc/v4"
)

// RelayChannel implements MessageChannel over a local connection to
// deskconnd's stream-relay listener, standing in for the real
// *webrtc.DataChannel. The backpressure methods are no-ops here: real
// backpressure is already enforced on xlink's side (see
// RelayWebRTCChannel), and Send/SendText block on conn's own OS write
// buffer instead.
type RelayChannel struct {
	conn net.Conn

	writeMu sync.Mutex

	startOnce sync.Once
	onMessage func(msg webrtc.DataChannelMessage)

	closeOnce sync.Once
	onClose   func()
	onError   func(err error)
}

func NewRelayChannel(conn net.Conn) *RelayChannel {
	return &RelayChannel{conn: conn}
}

func (c *RelayChannel) readLoop() {
	for {
		data, isText, err := ReadRelayFrame(c.conn)
		if err != nil {
			if c.onError != nil {
				c.onError(err)
			}
			c.signalClosed()
			return
		}
		if c.onMessage != nil {
			c.onMessage(webrtc.DataChannelMessage{Data: data, IsString: isText})
		}
	}
}

func (c *RelayChannel) signalClosed() {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
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
	c.onMessage = f
	c.startOnce.Do(func() { SafeGo(c.readLoop) })
}

func (c *RelayChannel) OnClose(f func())      { c.onClose = f }
func (c *RelayChannel) OnError(f func(error)) { c.onError = f }

func (c *RelayChannel) Close() error {
	c.signalClosed()
	return c.conn.Close()
}

func (c *RelayChannel) BufferedAmount() uint64                 { return 0 }
func (c *RelayChannel) SetBufferedAmountLowThreshold(_ uint64) {}
func (c *RelayChannel) OnBufferedAmountLow(_ func())           {}
