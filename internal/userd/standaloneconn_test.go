package userd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
)

// The daemon keeps a persistent connection to a standalone machine config.yml saves a
// password for, logging in with it without the account, and keeps it through signing out.
func TestDaemonConnectsToSavedStandaloneMachine(t *testing.T) {
	router, err := xconnd.NewDeviceRouter(common.StandaloneRealm)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	machine, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
	require.NoError(t, err)
	require.NoError(t, machine.Register(common.ProcedurePing, func(context.Context, *xconn.Invocation) *xconn.Result {
		return xconn.NewResult()
	}).Execute().Err)

	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	server := xconn.NewServer(router, xconnd.NewStandaloneAuthenticator(nil, map[string]string{"carol": "s3cret"}),
		&xconn.ServerConfig{})
	listener, err := server.ListenQUIC(t.Context(), "127.0.0.1:0", &cert)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	config := fmt.Sprintf("standalone:\n  - name: lab\n    url: quic://%s\n    cert_hash: %s\n"+
		"    username: carol\n    password: s3cret\n", listener.Address, common.CertificateFingerprint(cert))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"), []byte(config), 0600))
	realm := common.StandaloneMachineRealm("lab")
	t.Cleanup(func() { common.SetStandaloneTargetFor(realm, nil) })

	clients := userd.NewClientSessions() // never signed in to the account
	t.Cleanup(clients.DisconnectAll)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := clients.EnsureMachineSession(ctx, realm, dir)
	require.NoError(t, err)
	require.NoError(t, session.Call(common.ProcedurePing).ExecuteContext(ctx).Err)

	again, err := clients.EnsureMachineSession(ctx, realm, dir)
	require.NoError(t, err)
	require.Same(t, session, again, "the connection persists across uses")

	clients.Logout()
	_, ok := clients.SessionByRealm(realm)
	require.True(t, ok, "signing out of the account leaves standalone machines connected")
}
