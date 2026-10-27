//go:build linux

package deskconn_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

// fakeConn is a MountConnection that fails its calls once lost.
type fakeConn struct {
	done  chan struct{}
	calls atomic.Int32
	// dieOnCall loses the connection during the next call.
	dieOnCall atomic.Bool
	// failOnce fails the next call, the connection staying up.
	failOnce *atomic.Bool
}

func (c *fakeConn) conn() *deskconn.MountConnection {
	return &deskconn.MountConnection{
		Call: func(context.Context, []byte) ([]byte, error) {
			c.calls.Add(1)
			if c.failOnce.CompareAndSwap(true, false) {
				return nil, errors.New("connection closed unexpectedly")
			}
			if c.dieOnCall.Load() {
				close(c.done)
			}
			select {
			case <-c.done:
				return nil, errors.New("connection closed unexpectedly")
			default:
				return []byte("ok"), nil
			}
		},
		Done:  c.done,
		Close: func() {},
	}
}

func TestReconnectingMountCaller(t *testing.T) {
	var conns []*fakeConn
	connect := func(context.Context) (*deskconn.MountConnection, error) {
		c := &fakeConn{done: make(chan struct{}), failOnce: new(atomic.Bool)}
		conns = append(conns, c)
		return c.conn(), nil
	}
	call, closeConn, err := deskconn.ReconnectingMountCaller(context.Background(), connect)
	require.NoError(t, err)
	defer closeConn()

	request := func(req *common.MountRequest) []byte {
		payload, err := common.EncodeMountMessage(req, nil)
		require.NoError(t, err)
		return payload
	}
	stat := request(&common.MountRequest{Op: common.MountOpStat, Path: "a"})
	write := request(&common.MountRequest{Op: common.MountOpWrite, Handle: 1})

	_, err = call(context.Background(), stat)
	require.NoError(t, err)
	require.Len(t, conns, 1)

	t.Run("a lost connection is replaced", func(t *testing.T) {
		close(conns[len(conns)-1].done)
		_, err := call(context.Background(), stat)
		require.NoError(t, err)
		require.Len(t, conns, 2)
	})

	t.Run("an idempotent request is retried on a new connection", func(t *testing.T) {
		conns[len(conns)-1].dieOnCall.Store(true)
		_, err := call(context.Background(), stat)
		require.NoError(t, err)
		require.Len(t, conns, 3)
	})

	t.Run("an idempotent request is retried when the call fails on a live connection", func(t *testing.T) {
		// In default mode: the local daemon's connection to the machine was lost.
		conns[len(conns)-1].failOnce.Store(true)
		_, err := call(context.Background(), stat)
		require.NoError(t, err)
		require.Len(t, conns, 3, "over the same connection")
	})

	t.Run("a write is not retried", func(t *testing.T) {
		conns[len(conns)-1].dieOnCall.Store(true)
		_, err := call(context.Background(), write)
		require.Error(t, err)
		require.Len(t, conns, 3)

		_, err = call(context.Background(), stat)
		require.NoError(t, err, "but the next request reconnects")
		require.Len(t, conns, 4)
	})
}
