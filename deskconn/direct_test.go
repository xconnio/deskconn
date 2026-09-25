package deskconn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const testDirectDevice = "web1"

func TestNormalizeDirectAddress(t *testing.T) {
	require.Equal(t, "203.0.113.5:18080", deskconn.NormalizeDirectAddress("203.0.113.5"))
	require.Equal(t, "203.0.113.5:9000", deskconn.NormalizeDirectAddress("203.0.113.5:9000"))
	require.Equal(t, "[2001:db8::1]:18080", deskconn.NormalizeDirectAddress("2001:db8::1"))
}

func TestAddRemoveDirectDevice(t *testing.T) {
	cfgDir := t.TempDir()
	device := common.Device{Name: testDirectDevice, Realm: common.DirectRealmPrefix + testDirectDevice,
		Address: "203.0.113.5:18080"}

	require.NoError(t, deskconn.AddDirectDevice(cfgDir, device))
	require.Error(t, deskconn.AddDirectDevice(cfgDir, device), "duplicate name must be rejected")

	devices, err := common.DevicesFromCfg(cfgDir)
	require.NoError(t, err)
	require.Equal(t, []common.Device{device}, devices)

	require.NoError(t, deskconn.RemoveDirectDevice(cfgDir, testDirectDevice))
	require.Error(t, deskconn.RemoveDirectDevice(cfgDir, testDirectDevice))
	devices, err = common.DevicesFromCfg(cfgDir)
	require.NoError(t, err)
	require.Empty(t, devices)
}

// keyAuthenticator accepts cryptosign clients holding one public key.
type keyAuthenticator struct{ publicKey string }

func (a keyAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (a keyAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok && r.PublicKey() == a.publicKey {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("unknown publickey")
}

func TestConnectDirectDevice(t *testing.T) {
	cfgDir := t.TempDir()
	_, pub, _, err := common.EnsureDirectKey(cfgDir)
	require.NoError(t, err)

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	require.NoError(t, err)
	require.NoError(t, router.AddRealm(common.StandaloneRealm, &xconn.RealmConfig{}))
	tlsConfig, err := xconn.GenerateSelfSignedTLSConfig()
	require.NoError(t, err)
	listener, err := xconn.NewServer(router, keyAuthenticator{pub}, nil).ListenAndServeQUIC("127.0.0.1:0", tlsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	fingerprint := common.CertFingerprint(tlsConfig.Certificates[0].Certificate[0])
	device := common.Device{Name: testDirectDevice, Realm: common.DirectRealmPrefix + testDirectDevice,
		Address: listener.Addr().String(), Fingerprint: fingerprint}
	require.NoError(t, deskconn.AddDirectDevice(cfgDir, device))

	// Resolved by realm, as every CLI command does.
	sess, err := common.ConnectDeviceRealmQUIC(context.Background(), device.Realm, cfgDir)
	require.NoError(t, err)
	require.Equal(t, common.StandaloneRealm, sess.Details().Realm())
	_ = sess.Connection().Close()

	device.Fingerprint = common.CertFingerprint([]byte("some other certificate"))
	_, err = common.ConnectDirectQUIC(context.Background(), device, cfgDir)
	require.ErrorContains(t, err, "does not match pinned")

}
