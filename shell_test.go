package deskconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func setupDeskconn(t *testing.T) (*xconn.Session, *xconn.Session) {
	t.Helper()
	callee, caller := setupRouterAndConnectSessions(t)
	d := deskconn.NewDeskconn(nil, nil, nil, false, t.TempDir())
	require.NoError(t, d.Register(callee))
	return callee, caller
}
