//go:build linux

package deskconn

import (
	"context"
	"log"
	"sync"

	"github.com/xconnio/deskconn/internal/common"
)

// MountConnection is one connection a mount's requests go over.
type MountConnection struct {
	Call MountCaller
	// Done is closed once the connection is lost.
	Done  <-chan struct{}
	Close func()
}

// ReconnectingMountCaller sends requests over a connection from connect,
// connecting again when it's lost (the machine's daemon restarted, the
// network changed, ...) so the mount keeps working. Files open on the lost
// connection are gone with it: the machine closes them when the session ends.
// It connects once before returning; closeConn closes the current connection.
func ReconnectingMountCaller(ctx context.Context,
	connect func(context.Context) (*MountConnection, error)) (call MountCaller, closeConn func(), err error) {
	r := &reconnectingCaller{connect: connect}
	if _, err := r.current(ctx); err != nil {
		return nil, nil, err
	}
	return r.call, r.close, nil
}

type reconnectingCaller struct {
	connect func(context.Context) (*MountConnection, error)

	mu     sync.Mutex
	conn   *MountConnection
	closed bool
}

func (r *reconnectingCaller) current(ctx context.Context) (*MountConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, context.Canceled
	}
	if r.conn != nil {
		select {
		case <-r.conn.Done:
			r.conn.Close()
			r.conn = nil
			log.Printf("mount: connection lost, reconnecting")
		default:
			return r.conn, nil
		}
	}

	conn, err := r.connect(ctx)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

func (r *reconnectingCaller) call(ctx context.Context, payload []byte) ([]byte, error) {
	conn, err := r.current(ctx)
	if err != nil {
		return nil, err
	}
	result, err := conn.Call(ctx, payload)
	// A failed call is the connection's doing (the machine's own failures are
	// errnos in a result): either this connection was lost, or, in default
	// mode, the local daemon's to the machine was, which it replaces on the
	// next call. Either way, a retry goes over a new one.
	if err == nil || ctx.Err() != nil || !retryable(payload) {
		return result, err
	}

	conn, err = r.current(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Call(ctx, payload)
}

func (r *reconnectingCaller) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}

// retryable reports whether the request can be sent again after the
// connection it was sent on was lost, not knowing whether the machine ran it:
// whether it has run or not, running it again leaves the same result.
func retryable(payload []byte) bool {
	var req common.MountRequest
	if _, err := common.DecodeMountMessage(payload, &req); err != nil {
		return false
	}
	switch req.Op {
	case common.MountOpStat, common.MountOpReadDir, common.MountOpReadlink, common.MountOpStatfs:
		return true
	case common.MountOpRead:
		return req.Handle == 0 // a handle doesn't outlive its connection
	case common.MountOpOpen:
		return !req.Create && !req.Truncate
	}
	return false
}
