package deskconn

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
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

const (
	RelayKindQUIC   RelayKind = "quic"
	RelayKindWebRTC RelayKind = "webrtc"
)

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

func WriteRelayHeader(w io.Writer, h RelayHeader) error {
	return WriteMsg(w, h)
}

func ReadRelayHeader(r io.Reader) (RelayHeader, error) {
	var h RelayHeader
	err := ReadMsg(r, &h)
	return h, err
}

// relayFrameKind discriminates one RelayFrame from another on a
// RelayKindWebRTC connection.
type relayFrameKind byte

const (
	relayFrameBinary relayFrameKind = 0
	relayFrameText   relayFrameKind = 1
)

// maxRelayFrameSize bounds ReadRelayFrame's allocation.
const maxRelayFrameSize = 1 << 20 // 1 MiB

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

// relayBufferedHigh/Low bound how much data RelayWebRTCChannel lets pile up
// in the real channel's own SCTP send buffer before pausing reads from conn.
const (
	relayBufferedHigh = 512 * 1024
	relayBufferedLow  = 256 * 1024
)

// RelayWebRTCChannel splices channel's messages to/from conn as RelayFrames
// in both directions, respecting the real channel's send backpressure. If
// firstMessage is non-nil, it's relayed as the first frame.
func RelayWebRTCChannel(channel MessageChannel, conn net.Conn, firstMessage []byte) {
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
		select {
		case msgCh <- msg:
		case <-closed:
		}
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
				return
			}
		}
	})

	// conn -> channel, pausing whenever the real channel's own send buffer is
	// already full rather than queuing unboundedly on top of it.
	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(relayBufferedLow)
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

		for channel.BufferedAmount()+uint64(len(data)) > relayBufferedHigh {
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

// ServeStreamRelay accepts xlink's relayed local connections on ln and
// dispatches each to the feature it belongs to.
func (d *Deskconn) ServeStreamRelay(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		SafeGo(func() { d.handleRelayConn(conn) })
	}
}

// handleRelayConn reads the RelayHeader xlink wrote and resumes handling
// the connection where xlink's classification left off.
func (d *Deskconn) handleRelayConn(conn net.Conn) {
	header, err := ReadRelayHeader(conn)
	if err != nil {
		conn.Close()
		return
	}

	if header.Kind == RelayKindQUIC {
		d.DispatchQUICOp(header.Op, conn)
		return
	}

	// WebRTC-originated. VPN's first message is classification-only and
	// never relayed (see xlink's channel classification); every other
	// feature's first message -- its ephemeral public key -- is real
	// payload, relayed as the first frame.
	if header.Label == VPNChannelLabel {
		d.handleVPNChannel(NewRelayChannel(conn))
		return
	}

	firstMessage, _, err := ReadRelayFrame(conn)
	if err != nil {
		conn.Close()
		return
	}
	channel := NewRelayChannel(conn)

	switch header.Label {
	case ShellChannelLabel:
		d.HandleShellChannel("", channel, firstMessage)
	case PortForwardChannelLabel:
		d.HandlePortForwardChannel("", channel, firstMessage)
	case PortReverseChannelLabel:
		d.HandlePortReverseChannel("", channel, firstMessage)
	case AgentForwardChannelLabel:
		d.HandleAgentForwardChannel("", channel, firstMessage)
	case LogChannelLabel:
		d.HandleLogsChannel("", channel, firstMessage)
	default:
		d.HandleFileStreamChannel("", channel, firstMessage)
	}
}
