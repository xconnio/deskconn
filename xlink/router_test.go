package xlink_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/xlink"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const (
	testUser     = "carol"
	testPassword = "s3cret"
)

// startStandalone serves the standalone realm over yamux on a local port, like deskconnd
// --standalone, and returns its URL.
func startStandalone(t *testing.T, keys []string, passwords map[string]string) string {
	t.Helper()
	router := xlink.NewDeviceRouter(common.StandaloneRealm)
	t.Cleanup(router.Close)

	local, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
	require.NoError(t, err)
	resp := local.Register("io.xconn.test.echo", func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		return xconn.NewInvocationResult(inv.Args()...)
	}).Do()
	require.NoError(t, resp.Err)

	listener, err := common.ListenYamux("tcp://127.0.0.1:0", router, xlink.NewStandaloneAuthenticator(keys, passwords))
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return "tcp://" + listener.Addr().String()
}

func connectStandalone(t *testing.T, target *common.StandaloneTarget) (*common.DeviceConn, error) {
	t.Helper()
	authenticator, err := target.Authenticator()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return common.ConnectYamux(ctx, target.URL, common.StandaloneRealm, authenticator)
}

func TestStandaloneAuthenticatorPassword(t *testing.T) {
	url := startStandalone(t, nil, map[string]string{testUser: testPassword})

	conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, Password: testPassword})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.Equal(t, testUser, conn.Details().AuthID())
	resp := conn.Call("io.xconn.test.echo").Args("hello").Do()
	require.NoError(t, resp.Err)
	require.Equal(t, "hello", resp.Args()[0])

	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, Password: "wrong"})
	require.Error(t, err)
	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: "bob", Password: testPassword})
	require.Error(t, err)

	// No keys configured: cryptosign is refused.
	_, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, PrivateKey: priv})
	require.Error(t, err)
}

func TestStandaloneAuthenticatorKeysAndPasswords(t *testing.T) {
	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	url := startStandalone(t, []string{pub}, map[string]string{testUser: testPassword})

	conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: "anyone", PrivateKey: priv})
	require.NoError(t, err)
	_ = conn.Close()

	conn, err = connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, Password: testPassword})
	require.NoError(t, err)
	_ = conn.Close()
}

func TestStandaloneAuthenticatorKeysOnly(t *testing.T) {
	pub, _, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	url := startStandalone(t, []string{pub}, nil)

	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, Password: testPassword})
	require.Error(t, err)
}

func TestStandaloneTargetReadsPasswordOnce(t *testing.T) {
	url := startStandalone(t, nil, map[string]string{testUser: testPassword})
	reads := 0
	target := &common.StandaloneTarget{URL: url, AuthID: testUser, ReadPassword: func() (string, error) {
		reads++
		return testPassword, nil
	}}

	for range 2 {
		conn, err := connectStandalone(t, target)
		require.NoError(t, err)
		_ = conn.Close()
	}
	require.Equal(t, 1, reads)
}
