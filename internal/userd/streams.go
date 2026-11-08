package userd

import (
	"net"
	"sync"
)

// CloseStreams closes every active RPC stream still open, and refuses any
// opened after, returning how many it closed. Each feature handles it as its client
// going away, and xconn passes the close on to the client, which a shutdown needs:
// otherwise a stream client never learns the machine is gone and waits
// forever (a shell across desk self update, say).
func (d *Deskconn) CloseStreams() int {
	return d.streamConns.closeAll()
}

// streamConns tracks active feature streams for CloseStreams. Its
// zero value is ready to use.
type streamConns struct {
	mu     sync.Mutex
	conns  map[*trackedStreamConn]struct{}
	closed bool
}

// add tracks conn until it's closed, unless closeAll already ran.
func (r *streamConns) add(conn net.Conn) (net.Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false
	}
	if r.conns == nil {
		r.conns = make(map[*trackedStreamConn]struct{})
	}
	tracked := &trackedStreamConn{Conn: conn, set: r}
	r.conns[tracked] = struct{}{}
	return tracked, true
}

func (r *streamConns) remove(conn *trackedStreamConn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

func (r *streamConns) closeAll() int {
	r.mu.Lock()
	r.closed = true
	conns := make([]*trackedStreamConn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return len(conns)
}

// trackedStreamConn leaves its streamConns once closed.
type trackedStreamConn struct {
	net.Conn
	set *streamConns
}

func (c *trackedStreamConn) Close() error {
	c.set.remove(c)
	return c.Conn.Close()
}
