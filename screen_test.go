package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
)

func TestLock(t *testing.T) {
	ls := &deskconn.Screen{}

	err := ls.Lock()
	require.EqualError(t, err, "screen lock provider not initialized")
}

func TestIsLocked(t *testing.T) {
	ls := &deskconn.Screen{}

	_, err := ls.IsLocked()
	require.EqualError(t, err, "screen lock provider not initialized")
}
