package xlink_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/deskconn/xlink"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const testAuthID = "alice"

func TestLoadStandaloneConfigFromFile(t *testing.T) {
	cfgDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yml"), []byte(`standalone:
  enabled: true
  principals:
    - authid: alice
      authorized_keys: [k1]
`), 0600))

	config, err := xlink.LoadStandaloneConfig(cfgDir)
	require.NoError(t, err)
	require.True(t, config.Enabled)
	require.Equal(t, xlink.StandaloneDefaultListen, config.Listen)
	require.Equal(t, []common.StandalonePrincipal{{AuthID: testAuthID, AuthorizedKeys: []string{"k1"}}},
		config.Principals)
}

func TestLoadStandaloneConfigEnvOverrides(t *testing.T) {
	cfgDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yml"),
		[]byte("standalone:\n  principals:\n    - authid: alice\n      authorized_keys: [k1]\n"), 0600))
	t.Setenv("DESKCONN_STANDALONE", "true")
	t.Setenv("DESKCONN_STANDALONE_LISTEN", "127.0.0.1:9999")
	t.Setenv("DESKCONN_STANDALONE_KEYS", "alice:k2, bob:k3")

	config, err := xlink.LoadStandaloneConfig(cfgDir)
	require.NoError(t, err)
	require.True(t, config.Enabled)
	require.Equal(t, "127.0.0.1:9999", config.Listen)

	principals := xlink.StandalonePrincipals(config)
	require.Len(t, principals, 2)
	require.Equal(t, testAuthID, principals[0].AuthID)
	require.Equal(t, []string{"k1", "k2"}, principals[0].AuthorizedKeys)
	require.Equal(t, "bob", principals[1].AuthID)
	require.Equal(t, xlink.StandaloneAuthRole, principals[1].AuthRole)
}

func TestLoadStandaloneConfigDisabledByDefault(t *testing.T) {
	config, err := xlink.LoadStandaloneConfig(t.TempDir())
	require.NoError(t, err)
	require.False(t, config.Enabled)
}

func TestLoadStandaloneConfigInvalidEnv(t *testing.T) {
	t.Setenv("DESKCONN_STANDALONE_KEYS", "no-colon")
	_, err := xlink.LoadStandaloneConfig(t.TempDir())
	require.Error(t, err)

	t.Setenv("DESKCONN_STANDALONE_KEYS", "")
	t.Setenv("DESKCONN_STANDALONE", "maybe")
	_, err = xlink.LoadStandaloneConfig(t.TempDir())
	require.Error(t, err)
}

func TestLoadOrCreateCertIsStable(t *testing.T) {
	cfgDir := t.TempDir()

	first, err := xlink.LoadOrCreateCert(cfgDir)
	require.NoError(t, err)
	second, err := xlink.LoadOrCreateCert(cfgDir)
	require.NoError(t, err)
	require.Equal(t, common.CertFingerprint(first.Certificate[0]), common.CertFingerprint(second.Certificate[0]))

	info, err := os.Stat(filepath.Join(cfgDir, "standalone.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

// TestStandaloneRelaysAuthenticatedStream checks the whole standalone path: a client
// authenticated with a configured key gets its raw stream relayed to deskconnd's
// stream socket, and an unknown key is refused.
func TestStandaloneRelaysAuthenticatedStream(t *testing.T) {
	cfgDir, err := os.MkdirTemp("", "xlink") // short path: unix socket names are length-limited
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(cfgDir) })

	pubKey, privKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	config := &common.StandaloneConfig{
		Listen:     "127.0.0.1:0",
		Principals: []common.StandalonePrincipal{{AuthID: testAuthID, AuthorizedKeys: []string{pubKey}}},
	}

	// Stand in for deskconnd's stream-relay listener.
	streamSock, err := net.Listen("unix", filepath.Join(cfgDir, "xlink-streams.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = streamSock.Close() })

	listener, _, stop, err := xlink.StartStandalone(cfgDir, config)
	require.NoError(t, err)
	t.Cleanup(stop)

	connect := func(privateKey string) (*xconn.QUICSession, error) {
		authenticator, err := auth.NewCryptoSignAuthenticator(testAuthID, privateKey, nil)
		require.NoError(t, err)
		return xconn.ConnectQUIC(context.Background(), listener.Addr().String(), common.StandaloneRealm,
			&xconn.QUICDialerConfig{
				Authenticator: authenticator,
				TLSConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			})
	}

	_, otherPrivKey, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	_, err = connect(otherPrivKey)
	require.Error(t, err)

	sess, err := connect(privKey)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	stream, err := sess.OpenStream()
	require.NoError(t, err)
	require.NoError(t, common.WriteMsg(stream, common.RoutingFrame{Realm: common.StandaloneRealm,
		Op: common.FSOpShell}))
	_, err = stream.Write([]byte("payload"))
	require.NoError(t, err)
	require.NoError(t, stream.Close())

	relayed, err := streamSock.Accept()
	require.NoError(t, err)
	defer relayed.Close()
	require.NoError(t, relayed.SetDeadline(time.Now().Add(5*time.Second)))

	header, err := deskconnd.ReadRelayHeader(relayed)
	require.NoError(t, err)
	require.Equal(t, common.RelayKindQUIC, header.Kind)
	require.Equal(t, common.FSOpShell, header.Op)

	payload := make([]byte, len("payload"))
	_, err = io.ReadFull(relayed, payload)
	require.NoError(t, err)
	require.Equal(t, "payload", string(payload))
}
