package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

func requireClosed(t *testing.T, session *deskconn.MachineSession) {
	t.Helper()
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the session doesn't end it")
	}
}

func TestConnectMachineSessionThroughDaemon(t *testing.T) {
	directory, err := os.MkdirTemp("", "machinesession")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	router, listener, local, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(directory, "deskconn.sock"))
	require.NoError(t, err)
	t.Cleanup(router.Close)
	t.Cleanup(func() { _ = listener.Close(); _ = local.Close() })

	session, err := deskconn.ConnectMachineSession(t.Context(), directory, streamProxyTestRealm, "")
	require.NoError(t, err)
	require.True(t, session.Proxied, "the daemon's session, to call the machine through")
	session.Close()
	requireClosed(t, session)
}

func TestConnectMachineSessionDaemonNotRunning(t *testing.T) {
	_, err := deskconn.ConnectMachineSession(t.Context(), t.TempDir(), streamProxyTestRealm, "")
	require.Error(t, err)
}

func TestConnectMachineSessionRouted(t *testing.T) {
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	addOwnerRealm(t, router, common.StandaloneRealm)
	machineURL, certHash := listenQUIC(t, router, anyKeyAuthenticator{})
	_, key, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	realm := common.StandaloneMachineRealm("machine-session-routed")
	common.SetStandaloneTargetFor(realm, &common.StandaloneTarget{
		Name: "machine-session-routed", URL: machineURL, CertHash: certHash, AuthID: "client", PrivateKey: key,
	})

	session, err := deskconn.ConnectMachineSession(t.Context(), t.TempDir(), realm, deskconn.ModeRouted)
	require.NoError(t, err)
	require.False(t, session.Proxied, "a connection of the machine's own")
	session.Close()
	requireClosed(t, session)
}
