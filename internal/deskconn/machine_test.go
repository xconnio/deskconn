package deskconn_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn/bindings/go"
)

// connectedMachinesDaemon returns a session to a daemon whose connected machines are result.
func connectedMachinesDaemon(t *testing.T, result ...any) *xconn.Session {
	t.Helper()
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm(common.LocalRealm, xconn.DefaultRealmConfig()))
	backend, err := xconn.ConnectInMemory(router, common.LocalRealm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	require.NoError(t, backend.Register(common.ProcedureConnectedMachines,
		func(_ context.Context, _ *xconn.Invocation) *xconn.Result {
			return xconn.NewResult(result...)
		}).Execute().Err)
	caller, err := xconn.ConnectInMemory(router, common.LocalRealm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })
	return caller
}

func TestConnectedMachines(t *testing.T) {
	connected, err := deskconn.ConnectedMachines(connectedMachinesDaemon(t,
		map[string]any{"realm.office": "2026-11-26T10:00:00Z"}))
	require.NoError(t, err)
	require.Contains(t, connected, "realm.office")
	require.NotContains(t, connected, "realm.lab")

	_, err = deskconn.ConnectedMachines(connectedMachinesDaemon(t))
	require.Error(t, err, "no result")
	_, err = deskconn.ConnectedMachines(connectedMachinesDaemon(t, "not a map"))
	require.Error(t, err)
}
