package userd_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/deskconn/relay"
	"github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

const (
	cloudMachineRealm  = "machine-realm"
	cloudMachineAuthID = "machine"
	cloudUserAuthID    = "user"
	cloudUserRole      = "user"
)

// testCloud is a cloud router serving one machine, whose daemon comes and goes, and the
// config directory of a user logged in to it.
type testCloud struct {
	t            *testing.T
	rawSocketURI string
	machineKey   string
	cfgDirectory string
}

func freeAddress(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		require.NoError(t, err)
		defer conn.Close()
		return conn.LocalAddr().String()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().String()
}

func startTestCloud(t *testing.T) *testCloud {
	t.Helper()
	machinePublic, machinePrivate, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	userPublic, userPrivate, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)

	quicAddress, rawSocketAddress := freeAddress(t, "udp"), freeAddress(t, "tcp")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	router, err := relay.New(ctx, relay.Config{
		Machines: func(context.Context) ([]relay.Machine, error) {
			return []relay.Machine{{AuthID: cloudMachineAuthID, Realm: cloudMachineRealm}}, nil
		},
		Authenticate: func(request auth.Request) (auth.Response, error) {
			cryptosign, ok := request.(*auth.RequestCryptoSign)
			switch {
			case !ok:
			case request.AuthID() == cloudMachineAuthID && cryptosign.PublicKey() == machinePublic:
				return auth.NewResponse(cloudMachineAuthID, relay.MachineAuthRole(cloudMachineAuthID), 0)
			case request.AuthID() == cloudUserAuthID && cryptosign.PublicKey() == userPublic:
				return auth.NewResponse(cloudUserAuthID, cloudUserRole, 0)
			}
			return nil, errors.New("unknown key")
		},
		UserRoles:        []xconn.RealmRole{{Name: cloudUserRole, Permissions: relay.MachineUserPermissions()}},
		QUICAddress:      quicAddress,
		RawSocketAddress: rawSocketAddress,
	})
	require.NoError(t, err)
	go func() { _ = router.Serve(ctx) }()
	t.Setenv("DESKCONN_CLOUD_QUIC_ADDRESS", quicAddress)

	cfgDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfgDirectory, "id_ed25519"),
		[]byte(userPrivate+" "+cloudUserAuthID), 0o600))

	cloud := &testCloud{t: t, rawSocketURI: "rs://" + rawSocketAddress, machineKey: machinePrivate,
		cfgDirectory: cfgDirectory}
	require.Eventually(t, func() bool {
		conn, err := common.ConnectMachineRealmQUIC(context.Background(), cloudMachineRealm, cfgDirectory)
		if err == nil {
			_ = conn.Close()
			_ = conn.Connection().Close()
		}
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "the router serves QUIC")
	return cloud
}

// procedureTestEcho is an encrypted procedure of the test machine's, which returns its
// request.
const procedureTestEcho = "io.xconn.deskconn.test.echo"

// machineOnline brings the machine's daemon online: answering ping (by its name before
// procedures lost their "deskconnd." part, if legacy), exchanging keys like userd's does,
// and serving procedureTestEcho with them. It goes offline when the returned func is called.
func (c *testCloud) machineOnline(legacy bool) (offline func()) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := xconn.ConnectCryptosign(ctx, c.rawSocketURI, cloudMachineRealm, cloudMachineAuthID, c.machineKey)
	require.NoError(c.t, err)

	ping := common.ProcedurePing
	if legacy {
		ping = "io.xconn.deskconn.deskconnd.ping"
	}
	var mu sync.Mutex
	keys := map[uint64]*common.SessionKeys{} // by caller, from the machine's side
	handlers := map[string]xconn.InvocationHandler{
		ping: func(context.Context, *xconn.Invocation) *xconn.Result {
			return xconn.NewResult()
		},
		common.ProcedureKeyExchange: func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
			clientPublicKey, err := inv.ArgBytes(0)
			require.NoError(c.t, err)
			publicKey, sendKey, receiveKey, err := common.ServerKeyExchange(clientPublicKey)
			require.NoError(c.t, err)
			time.Sleep(50 * time.Millisecond) // long enough for exchanges at once to overlap
			mu.Lock()
			keys[inv.Caller()] = &common.SessionKeys{SendKey: sendKey, ReceiveKey: receiveKey}
			mu.Unlock()
			return xconn.NewResult(publicKey)
		},
		// As a machine's procedures can fail.
		common.ProcedureMachineInfo: func(context.Context, *xconn.Invocation) *xconn.Result {
			return xconn.NewResultError(common.ErrOperationFailed, "no info today")
		},
		procedureTestEcho: func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
			mu.Lock()
			enc := keys[inv.Caller()]
			mu.Unlock()
			encrypted, err := inv.ArgBytes(0)
			require.NoError(c.t, err)
			request, err := common.DecryptPayload(encrypted, enc.ReceiveKey)
			if err != nil {
				return xconn.NewResultError(common.ErrInvalidArgument, "failed to decrypt: "+err.Error())
			}
			result, err := common.EncryptPayload(request, enc.SendKey)
			require.NoError(c.t, err)
			return xconn.NewResult(result)
		},
	}
	for procedure, handler := range handlers {
		require.NoError(c.t, session.Register(procedure, handler).Execute().Err, procedure)
	}

	var once sync.Once
	offline = func() { once.Do(func() { _ = session.Leave() }) }
	c.t.Cleanup(offline)
	return offline
}

func connectedRealms(sessions *userd.ClientSessions) []string {
	var realms []string
	for realm := range sessions.MachineSessions() {
		realms = append(realms, realm)
	}
	return realms
}

func TestConnectingToAnOfflineMachine(t *testing.T) {
	cloud := startTestCloud(t)
	sessions := userd.NewClientSessions()

	_, err := sessions.EnsureMachineSession(context.Background(), cloudMachineRealm, cloud.cfgDirectory)
	require.ErrorIs(t, err, userd.ErrMachineOffline, "the router takes the session, but no machine is there")
	require.Empty(t, connectedRealms(sessions), "so it isn't connected")
}

func TestConnectingToAnOnlineMachine(t *testing.T) {
	cloud := startTestCloud(t)
	cloud.machineOnline(false)
	sessions := userd.NewClientSessions()

	// Callers at once share one connection.
	results := make([]*xconn.Session, 5)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := sessions.EnsureMachineSession(context.Background(), cloudMachineRealm, cloud.cfgDirectory)
			require.NoError(t, err)
			results[i] = session
		}()
	}
	wg.Wait()
	for _, session := range results {
		require.Same(t, results[0], session)
	}
	require.Equal(t, []string{cloudMachineRealm}, connectedRealms(sessions))
}

func TestConnectingToAMachineNotUpdated(t *testing.T) {
	cloud := startTestCloud(t)
	cloud.machineOnline(true)

	_, err := userd.NewClientSessions().EnsureMachineSession(context.Background(), cloudMachineRealm,
		cloud.cfgDirectory)
	require.NoError(t, err, "it answers to ping by its old name")
}

func TestLostConnection(t *testing.T) {
	cloud := startTestCloud(t)
	offline := cloud.machineOnline(false)
	sessions := userd.NewClientSessions()
	session, err := sessions.EnsureMachineSession(context.Background(), cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)

	// Not logged in, as after the daemon starts unless told: it isn't reconnected, but it
	// isn't left looking connected either.
	time.Sleep(time.Second) // the P2P upgrade fails (the test machine doesn't answer offers)
	require.NoError(t, session.Close())
	require.Eventually(t, func() bool { return len(connectedRealms(sessions)) == 0 }, 5*time.Second,
		20*time.Millisecond, "the lost connection is forgotten")

	// Logged in, it reconnects once the machine is back, and not before.
	sessions.Login()
	session, err = sessions.EnsureMachineSession(context.Background(), cloudMachineRealm, cloud.cfgDirectory)
	require.NoError(t, err)
	time.Sleep(time.Second)
	offline()
	require.NoError(t, session.Close())
	require.Eventually(t, func() bool { return len(connectedRealms(sessions)) == 0 }, 5*time.Second,
		20*time.Millisecond)
	require.Never(t, func() bool { return len(connectedRealms(sessions)) > 0 }, 3*time.Second,
		100*time.Millisecond, "the machine is offline")

	cloud.machineOnline(false)
	require.Eventually(t, func() bool { return len(connectedRealms(sessions)) == 1 }, 20*time.Second,
		100*time.Millisecond, "back once the machine is")
}

func TestConnectedSinceKeptAcrossUpgrade(t *testing.T) {
	router, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, router.AddRealm("realm1", xconn.DefaultRealmConfig()))
	quic, err := xconn.ConnectInMemory(router, "realm1")
	require.NoError(t, err)
	p2p, err := xconn.ConnectInMemory(router, "realm1")
	require.NoError(t, err)

	sessions := userd.NewClientSessions()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessions.StoreMachineSession(cloudMachineRealm, quic, nil, nil, ctx, cancel)
	since := sessions.MachineSessions()[cloudMachineRealm]
	time.Sleep(1100 * time.Millisecond)
	sessions.StoreMachineSession(cloudMachineRealm, p2p, nil, nil, ctx, cancel)
	require.Equal(t, since, sessions.MachineSessions()[cloudMachineRealm], "only the transport changed")
}

func TestProxiedCallsAtOnce(t *testing.T) {
	cloud := startTestCloud(t)
	cloud.machineOnline(false)
	sessions := userd.NewClientSessions()

	local, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, local.AddRealm(common.LocalRealm, xconn.DefaultRealmConfig()))
	daemon, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)
	require.NoError(t, daemon.Register(common.ProcedureProxyFileOp,
		userd.ProxyFileOpHandler(sessions, cloud.cfgDirectory)).Execute().Err)
	cli, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)

	// Calls at once on a machine session not used yet share one key exchange, and so none
	// is refused for its keys.
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := cli.Call(common.ProcedureProxyFileOp).Args(cloudMachineRealm, procedureTestEcho, []byte("hi")).Execute()
			require.NoError(t, resp.Err)
			result, err := resp.ArgBytes(0)
			require.NoError(t, err)
			require.Equal(t, "hi", string(result))
		}()
	}
	wg.Wait()
}

func TestFailedProxiedCallKeepsTheConnection(t *testing.T) {
	cloud := startTestCloud(t)
	cloud.machineOnline(false)
	sessions := userd.NewClientSessions()

	local, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, local.AddRealm(common.LocalRealm, xconn.DefaultRealmConfig()))
	daemon, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)
	require.NoError(t, daemon.Register(common.ProcedureProxyMachineInfo,
		userd.ProxyMachineInfoHandler(sessions, cloud.cfgDirectory)).Execute().Err)
	cli, err := xconn.ConnectInMemory(local, common.LocalRealm)
	require.NoError(t, err)

	resp := cli.Call(common.ProcedureProxyMachineInfo).Args(cloudMachineRealm).Execute()
	require.ErrorContains(t, resp.Err, "no info today")
	require.Equal(t, []string{cloudMachineRealm}, connectedRealms(sessions),
		"the machine failing a call doesn't end the connection to it")
}
