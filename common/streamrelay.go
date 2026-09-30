package common

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// MessageChannel is the subset of *webrtc.DataChannel's API every raw
// data-channel feature actually uses. *webrtc.DataChannel satisfies it
// directly; deskconnd, which never sees a real one, satisfies it with a
// RelayChannel backed by a local unix connection to xlink instead.
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

// RelayKind says whether a local relay connection carries an ordered byte
// stream (QUIC) or discrete messages (WebRTC) -- see RelayHeader.
type RelayKind string

// RelayHeader is the first message xlink writes on every local connection
// it opens to deskconnd's stream-relay listener: it tells deskconnd which
// feature owns the rest of the connection. For RelayKindQUIC, Op is set and
// the remainder is the QUIC stream's bytes, untouched. For RelayKindWebRTC,
// Label is set and the remainder is a sequence of RelayFrames (see
// WriteRelayFrame/ReadRelayFrame), since a raw byte splice would lose
// WebRTC's message boundaries.
type RelayHeader struct {
	Kind    RelayKind `json:"kind"`
	Op      FSOp      `json:"op,omitempty"`
	Label   string    `json:"label,omitempty"`
	Ordered bool      `json:"ordered"`
}

// relayFrameKind discriminates one RelayFrame from another on a
// RelayKindWebRTC connection.
type relayFrameKind byte

// WriteRelayFrame writes one WebRTC message (and whether it was sent as
// text or binary) as one frame on a RelayKindWebRTC local connection.
func WriteRelayFrame(w io.Writer, data []byte, isText bool) error {
	kind := relayFrameBinary
	if isText {
		kind = relayFrameText
	}
	var header [5]byte
	header[0] = byte(kind)
	binary.BigEndian.PutUint32(header[1:], uint32(len(data))) //nolint:gosec
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// ReadRelayFrame reads one frame written by WriteRelayFrame.
func ReadRelayFrame(r io.Reader) (data []byte, isText bool, err error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, false, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > maxRelayFrameSize {
		return nil, false, fmt.Errorf("relay frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, false, err
	}
	return buf, header[0] == byte(relayFrameText), nil
}

// WebrtcBackpressure wires up the OnClose/OnError/OnBufferedAmountLow
// callbacks shared by anything streaming a lot of binary messages over
// channel, in either direction.
func WebrtcBackpressure(channel MessageChannel) (closed <-chan struct{}, sendReady <-chan struct{}) {
	closedCh := make(chan struct{})
	var closeOnce sync.Once
	markClosed := func() { closeOnce.Do(func() { close(closedCh) }) }
	channel.OnClose(markClosed)
	channel.OnError(func(error) {
		markClosed()
	})

	readyCh := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(FileStreamBufferedLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	return closedCh, readyCh
}

// RelayWebRTCChannel splices channel's messages to/from conn as RelayFrames
// in both directions, respecting the real channel's send backpressure. If
// firstMessage is non-nil, it's relayed as the first frame.
func RelayWebRTCChannel(channel MessageChannel, conn net.Conn, firstMessage []byte) {
	RelayWebRTCChannelBuffered(channel, conn, firstMessage, RelayBufferedHigh, RelayBufferedLow)
}

// RelayWebRTCChannelBuffered is RelayWebRTCChannel with its own send buffer limits.
func RelayWebRTCChannelBuffered(channel MessageChannel, conn net.Conn, firstMessage []byte, high, low uint64) {
	defer conn.Close()

	if firstMessage != nil {
		if err := WriteRelayFrame(conn, firstMessage, true); err != nil {
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
		DeliverUnlessClosed(msgCh, msg, closed)
	})

	// channel -> conn
	SafeGo(func() {
		for {
			select {
			case msg := <-msgCh:
				if err := WriteRelayFrame(conn, msg.Data, msg.IsString); err != nil {
					_ = channel.Close()
					return
				}
			case <-closed:
				// Relay what arrived before the close, then close conn too.
				for {
					select {
					case msg := <-msgCh:
						if WriteRelayFrame(conn, msg.Data, msg.IsString) != nil {
							_ = conn.Close()
							return
						}
					default:
						_ = conn.Close()
						return
					}
				}
			}
		}
	})

	// conn -> channel, pausing whenever the real channel's own send buffer is
	// already full rather than queuing unboundedly on top of it.
	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(low)
	channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	for {
		data, isText, err := ReadRelayFrame(conn)
		if err != nil {
			_ = channel.Close()
			return
		}

		for channel.BufferedAmount()+uint64(len(data)) > high {
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

// DeliverUnlessClosed sends v on ch, giving up only if ch is full and closed has ended. A
// plain select on both could drop a message that arrives just as its channel closes.
func DeliverUnlessClosed[T any](ch chan<- T, v T, closed <-chan struct{}) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case ch <- v:
	case <-closed:
	}
}

// DataChannelOpener is what's needed of a P2P session to open a data channel on it.
type DataChannelOpener interface {
	OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error)
}

// OpenDataChannel opens a reliable, ordered data channel on sess and waits for it to open.
func OpenDataChannel(sess DataChannelOpener, label string) (*webrtc.DataChannel, error) {
	channel, err := sess.OpenChannel(label, nil)
	if err != nil {
		return nil, err
	}

	closedCh := make(chan struct{})
	var closedOnce sync.Once
	signalClosed := func() { closedOnce.Do(func() { close(closedCh) }) }
	channel.OnClose(signalClosed)
	channel.OnError(func(error) { signalClosed() })

	openCh := make(chan struct{})
	channel.OnOpen(func() { close(openCh) })

	select {
	case <-openCh:
		return channel, nil
	case <-closedCh:
		return nil, fmt.Errorf("remote closed the %q channel before it opened", label)
	case <-time.After(P2PRequestTimeout):
		_ = channel.Close()
		return nil, fmt.Errorf("timed out opening the %q channel", label)
	}
}

// SpliceConns copies between a and b until either side ends, then closes both.
func SpliceConns(a, b net.Conn) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = a.Close()
			_ = b.Close()
			// Closing a multiplexed stream only ends its write side: don't wait for the peer.
			_ = a.SetReadDeadline(time.Now())
			_ = b.SetReadDeadline(time.Now())
		})
	}
	done := make(chan struct{})
	SafeGo(func() {
		_, _ = io.Copy(a, b)
		closeBoth()
		close(done)
	})
	_, _ = io.Copy(b, a)
	closeBoth()
	<-done
}
