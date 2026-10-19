package userd

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// streamOpReadTimeout bounds how long ReadStreamOp waits for the leading
// RoutingFrame before giving up on a stalled/malicious stream.
const streamOpReadTimeout = 30 * time.Second

// ReadStreamOp reads the leading RoutingFrame off a freshly accepted raw
// stream and returns which feature the rest of the stream belongs to. The
// routing frame is consumed; whatever comes after it on stream is untouched
// and ready for that feature to read.
func ReadStreamOp(stream net.Conn) (common.FSOp, error) {
	_ = stream.SetReadDeadline(time.Now().Add(streamOpReadTimeout))
	var route common.RoutingFrame
	if err := common.ReadMsg(stream, &route); err != nil {
		return "", err
	}
	_ = stream.SetReadDeadline(time.Time{})
	return route.Op, nil
}

// ServeStreamRelay accepts xlink's relayed local connections on ln and
// dispatches each to the feature it belongs to.
func (d *Deskconn) ServeStreamRelay(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tracked, ok := d.relayConns.add(conn)
		if !ok {
			_ = conn.Close()
			continue
		}
		common.SafeGo(func() { d.handleRelayConn(tracked) })
	}
}

// CloseStreams closes every relayed stream and channel still open, and refuses any
// relayed after, returning how many it closed. Each feature handles it as its client
// going away, and xlink passes the close on to the client, which a shutdown needs:
// otherwise a client on a P2P channel never learns the machine is gone and waits
// forever (a shell across desk self update, say).
func (d *Deskconn) CloseStreams() int {
	return d.relayConns.closeAll()
}

// relayConns is the set of relayed connections still open, for CloseStreams. Its
// zero value is ready to use.
type relayConns struct {
	mu     sync.Mutex
	conns  map[*trackedRelayConn]struct{}
	closed bool
}

// add tracks conn until it's closed, unless closeAll already ran.
func (r *relayConns) add(conn net.Conn) (net.Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	if r.conns == nil {
		r.conns = make(map[*trackedRelayConn]struct{})
	}
	tracked := &trackedRelayConn{Conn: conn, set: r}
	r.conns[tracked] = struct{}{}
	return tracked, true
}

func (r *relayConns) remove(conn *trackedRelayConn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

func (r *relayConns) closeAll() int {
	r.mu.Lock()
	r.closed = true
	conns := make([]*trackedRelayConn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return len(conns)
}

// trackedRelayConn leaves its relayConns once closed.
type trackedRelayConn struct {
	net.Conn
	set *relayConns
}

func (c *trackedRelayConn) Close() error {
	c.set.remove(c)
	return c.Conn.Close()
}

// handleRelayConn reads the xconnd.RelayHeader and classifies the connection:
// a raw stream by its leading RoutingFrame, a WebRTC data channel by its
// label or, failing that, its first message.
func (d *Deskconn) handleRelayConn(conn net.Conn) {
	header, err := xconnd.ReadRelayHeader(conn)
	if err != nil {
		conn.Close()
		return
	}

	if header.Kind == xconnd.RelayKindStream {
		op, err := ReadStreamOp(conn)
		if err != nil {
			conn.Close()
			return
		}
		d.DispatchQUICOp(op, conn)
		return
	}

	// WebRTC-originated: xlink relays the channel's first message as the first frame.
	firstMessage, _, err := common.ReadRelayFrame(conn)
	if err != nil {
		conn.Close()
		return
	}
	channel := common.NewRelayChannel(conn)

	switch header.Label {
	case common.ShellChannelLabel:
		d.HandleShellChannel("", channel, firstMessage)
	case common.PortForwardChannelLabel:
		d.HandlePortForwardChannel("", channel, firstMessage)
	case common.PortReverseChannelLabel:
		d.HandlePortReverseChannel("", channel, firstMessage)
	case common.AgentForwardChannelLabel:
		d.HandleAgentForwardChannel("", channel, firstMessage)
	case common.LogChannelLabel:
		d.HandleLogsChannel("", channel, firstMessage)
	default:
		// A VPN channel's first message only identifies it; every other
		// unlabeled channel is a file stream, whose first message -- its
		// ephemeral public key -- is real payload.
		var probe common.VPNOpenFrame
		if json.Unmarshal(firstMessage, &probe) == nil && probe.Type == common.VPNFrameOpen {
			d.handleVPNChannel(channel)
			return
		}
		d.HandleFileStreamChannel("", channel, firstMessage)
	}
}
