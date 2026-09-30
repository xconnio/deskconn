package deskconnd_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/deskconnd"
)

func newTestQUICConn(closed *int) *deskconnd.QUICConn {
	return deskconnd.NewQUICConn(func() error { *closed++; return nil })
}

func TestQUICConnRetiredWhileUnusedClosesAtOnce(t *testing.T) {
	var closed int
	q := newTestQUICConn(&closed)

	q.Retire()
	require.Equal(t, 1, closed)
	require.False(t, q.Acquire(), "a closed connection can't be used")
}

// An operation that started on QUIC keeps it open past the P2P upgrade; it closes when
// the last one ends.
func TestQUICConnRetiredWhileUsedClosesAfterLastUser(t *testing.T) {
	var closed int
	q := newTestQUICConn(&closed)

	require.True(t, q.Acquire())
	require.True(t, q.Acquire())
	q.Retire()
	require.Zero(t, closed)
	require.True(t, q.Acquire(), "still usable by the operations holding it")

	q.Release()
	q.Release()
	require.Zero(t, closed)
	q.Release()
	require.Equal(t, 1, closed)
	require.False(t, q.Acquire())
}

func TestQUICConnUnretiredStaysOpenWhenUnused(t *testing.T) {
	var closed int
	q := newTestQUICConn(&closed)

	require.True(t, q.Acquire())
	q.Release()
	require.Zero(t, closed, "QUIC persists when the P2P upgrade didn't happen")

	q.Close()
	q.Close()
	require.Equal(t, 1, closed)
}
