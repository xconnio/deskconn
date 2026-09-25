package common

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

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
