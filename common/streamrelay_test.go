package common_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

// fakeChannel is a MessageChannel the test drives by hand: deliver plays a
// message arriving from the remote end, remoteClose the remote end closing.
type fakeChannel struct {
	mu        sync.Mutex
	onMessage func(webrtc.DataChannelMessage)
	onClose   func()
}

func (c *fakeChannel) OnMessage(f func(webrtc.DataChannelMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessage = f
}

func (c *fakeChannel) OnClose(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onClose = f
}

func (c *fakeChannel) ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onMessage != nil && c.onClose != nil
}

func (c *fakeChannel) deliver(data []byte) {
	c.mu.Lock()
	f := c.onMessage
	c.mu.Unlock()
	f(webrtc.DataChannelMessage{Data: data})
}

func (c *fakeChannel) remoteClose() {
	c.mu.Lock()
	f := c.onClose
	c.mu.Unlock()
	f()
}

func (c *fakeChannel) Send([]byte) error                    { return nil }
func (c *fakeChannel) SendText(string) error                { return nil }
func (c *fakeChannel) OnError(func(error))                  {}
func (c *fakeChannel) Close() error                         { return nil }
func (c *fakeChannel) BufferedAmount() uint64               { return 0 }
func (c *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}
func (c *fakeChannel) OnBufferedAmountLow(func())           {}

// A channel closed by its remote end must end the relayed connection too, after
// flushing what arrived before the close -- otherwise the far side (e.g. the CLI
// behind deskconnd's stream proxy) only notices on its next write.
func TestRelayWebRTCChannelPropagatesRemoteClose(t *testing.T) {
	channel := &fakeChannel{}
	relaySide, localSide := net.Pipe()
	go common.RelayWebRTCChannel(channel, relaySide, nil)
	require.Eventually(t, channel.ready, time.Second, time.Millisecond)

	go func() {
		channel.deliver([]byte("last words"))
		channel.remoteClose()
	}()

	_ = localSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, _, err := common.ReadRelayFrame(localSide)
	require.NoError(t, err)
	require.Equal(t, "last words", string(data))

	_, _, err = common.ReadRelayFrame(localSide)
	require.ErrorIs(t, err, io.EOF, "the relayed connection must close once the channel does")
}

// A message and its channel's close can be ready at the same time; the message must
// still be delivered and received, not dropped by select's random choice.
func TestMessagesArrivingAtCloseAreKept(t *testing.T) {
	for range 200 {
		closed := make(chan struct{})
		close(closed)

		ch := make(chan int, 1)
		common.DeliverUnlessClosed(ch, 42, closed)
		v, err := common.RecvPriority(ch, closed, time.Second)
		require.NoError(t, err)
		require.Equal(t, 42, v)

		_, err = common.RecvPriority(ch, closed, time.Second)
		require.ErrorIs(t, err, io.ErrClosedPipe)
	}
}
