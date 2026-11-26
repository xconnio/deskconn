package common_test

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

const (
	testUser     = "carol"
	testPassword = "s3cret"
)

// startStandalone serves the standalone realm over QUIC and WebTransport on local ports,
// with xconn's standalone authenticator and a self-signed certificate like userd
// --standalone, and returns the QUIC and WebTransport URLs and the certificate fingerprint.
func startStandalone(t *testing.T, keys []string, passwords map[string]string) (quicURL, wtURL,
	certHash string) {
	t.Helper()
	router, err := xconnd.NewDeviceRouter(common.StandaloneRealm)
	require.NoError(t, err)
	t.Cleanup(router.Close)

	local, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
	require.NoError(t, err)
	resp := local.Register("com.deskconn.test.echo", func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		return xconn.NewResult(inv.Args()...)
	}).Execute()
	require.NoError(t, resp.Err)

	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	server := xconn.NewServer(router, xconnd.NewStandaloneAuthenticator(keys, passwords), &xconn.ServerConfig{})

	quicListener, err := server.ListenQUIC(t.Context(), "127.0.0.1:0", &tlsConfig.Certificates[0])
	require.NoError(t, err)
	t.Cleanup(func() { _ = quicListener.Close() })
	wtListener, err := server.ListenWebTransport(
		t.Context(), "127.0.0.1:0", xconnd.WebTransportPath, tlsConfig.Certificates[0],
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = wtListener.Close() })

	return "quic://" + quicListener.Address, "https://" + wtListener.Address,
		common.CertificateFingerprint(cert)
}

func connectStandalone(t *testing.T, target *common.StandaloneTarget) (*xconnd.MachineConn, error) {
	t.Helper()
	authenticator, err := target.Authenticator()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return xconnd.ConnectStandalone(ctx, target.URL, target.CertHash, common.StandaloneRealm, authenticator)
}

func TestParseStandaloneURL(t *testing.T) {
	u, err := xconnd.ParseStandaloneURL("quic://203.0.113.5:18080")
	require.NoError(t, err)
	require.Equal(t, "quic://203.0.113.5:18080", u.String())

	u, err = xconnd.ParseStandaloneURL("https://example.com:18081")
	require.NoError(t, err)
	require.Equal(t, "https://example.com:18081"+xconnd.WebTransportPath, u.String())

	u, err = xconnd.ParseStandaloneURL("https://example.com:443/custom")
	require.NoError(t, err)
	require.Equal(t, "/custom", u.Path)

	for _, bad := range []string{"tcp://host:1", "unix:///tmp/d.sock", "quic://host", "https://host", "::"} {
		_, err := xconnd.ParseStandaloneURL(bad)
		require.Error(t, err, bad)
	}
}

func TestStandaloneTransports(t *testing.T) {
	quicURL, wtURL, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword})
	for _, url := range []string{quicURL, wtURL} {
		conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash, AuthID: testUser,
			Password: testPassword})
		require.NoError(t, err, url)
		resp := conn.Call("com.deskconn.test.echo").Args("hello").Execute()
		require.NoError(t, resp.Err, url)
		require.Equal(t, "hello", resp.Args[0])

		require.NoError(t, conn.Close())
	}
}

func TestStandaloneCertificateVerification(t *testing.T) {
	quicURL, wtURL, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword})
	wrongHash := strings.Repeat("0", len(certHash))
	for _, url := range []string{quicURL, wtURL} {
		// Self-signed and not pinned: rejected by normal CA verification.
		_, err := connectStandalone(t, &common.StandaloneTarget{URL: url, AuthID: testUser, Password: testPassword})
		require.Error(t, err, url)

		// Pinned to another certificate: rejected.
		_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: wrongHash, AuthID: testUser,
			Password: testPassword})
		require.ErrorContains(t, err, "does not match", url)

		// Pinned, in the colon-separated uppercase form some tools print: accepted.
		var pairs []string
		for i := 0; i < len(certHash); i += 2 {
			pairs = append(pairs, strings.ToUpper(certHash[i:i+2]))
		}
		conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: strings.Join(pairs, ":"),
			AuthID: testUser, Password: testPassword})
		require.NoError(t, err, url)
		_ = conn.Close()
	}
}

func TestStandaloneAuthenticatorPassword(t *testing.T) {
	url, _, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword})

	conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, Password: testPassword})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.Equal(t, testUser, conn.Details().AuthID())
	resp := conn.Call("com.deskconn.test.echo").Args("hello").Execute()
	require.NoError(t, resp.Err)
	require.Equal(t, "hello", resp.Args[0])

	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, Password: "wrong"})
	require.Error(t, err)
	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: "bob", Password: testPassword})
	require.Error(t, err)

	// No keys configured: cryptosign is refused.
	_, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, PrivateKey: priv})
	require.Error(t, err)
}

func TestStandaloneAuthenticatorKeysAndPasswords(t *testing.T) {
	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	url, _, certHash := startStandalone(t, []string{pub}, map[string]string{testUser: testPassword})

	conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: "anyone", PrivateKey: priv})
	require.NoError(t, err)
	_ = conn.Close()

	conn, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, Password: testPassword})
	require.NoError(t, err)
	_ = conn.Close()
}

func TestStandaloneAuthenticatorKeysOnly(t *testing.T) {
	pub, _, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	url, _, certHash := startStandalone(t, []string{pub}, nil)

	_, err = connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, Password: testPassword})
	require.Error(t, err)
}

// A target saved with neither a private key nor a password (by an older desk machine add)
// can't log in, and says how to fix that rather than asking for a password.
func TestStandaloneTargetWithoutLogin(t *testing.T) {
	target := common.StandaloneMachine{Name: "lab", URL: "quic://lab:18080"}.Target()
	_, err := target.Authenticator()
	require.ErrorContains(t, err, "standalone machine lab has no saved private key or password")
	require.ErrorContains(t, err, "desk machine add lab --url quic://lab:18080")
}

func TestStandaloneMachineRealms(t *testing.T) {
	const otherUser = "dave"
	url, _, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword, otherUser: "pa55"})
	common.SetStandaloneTargetFor(common.StandaloneMachineRealm("one"),
		&common.StandaloneTarget{URL: url, CertHash: certHash, AuthID: testUser, Password: testPassword})
	common.SetStandaloneTargetFor(common.StandaloneMachineRealm("two"),
		&common.StandaloneTarget{URL: url, CertHash: certHash, AuthID: otherUser, Password: "pa55"})
	t.Cleanup(func() {
		common.SetStandaloneTargetFor(common.StandaloneMachineRealm("one"), nil)
		common.SetStandaloneTargetFor(common.StandaloneMachineRealm("two"), nil)
	})

	_, ok := common.StandaloneTargetFor(common.StandaloneRealm)
	require.False(t, ok, "only the machines named are targets")
	require.Equal(t, common.StandaloneRealm, common.ServedRealm(common.StandaloneMachineRealm("one")))
	require.Equal(t, "realm.office", common.ServedRealm("realm.office"))

	// Both at once, each as its own user, on the realm the machine serves.
	for name, user := range map[string]string{"one": testUser, "two": otherUser} {
		conn, err := xconnd.ConnectMachineRealmQUIC(context.Background(), common.StandaloneMachineRealm(name), t.TempDir())
		require.NoError(t, err, name)
		t.Cleanup(func() { _ = conn.Close() })
		require.Equal(t, user, conn.Details().AuthID(), name)
		require.Equal(t, common.StandaloneRealm, conn.Details().Realm(), name)
	}
}

// The daemon learns a standalone machine, and how to log in to it, from config.yml.
func TestLoadStandaloneTarget(t *testing.T) {
	dir := t.TempDir()
	config := `standalone:
  - name: keyed
    url: quic://lab:18080
    private_key: cd
  - name: passworded
    url: quic://lab:18080
    username: dave
    password: secret
  - name: asks
    url: quic://lab:18080
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yml"), []byte(config), 0600))
	t.Cleanup(func() {
		for _, name := range []string{"keyed", "passworded", "asks"} {
			common.SetStandaloneTargetFor(common.StandaloneMachineRealm(name), nil)
		}
	})

	passworded := common.StandaloneMachineRealm("passworded")
	require.NoError(t, common.LoadStandaloneTarget(dir, passworded))
	target, ok := common.StandaloneTargetFor(passworded)
	require.True(t, ok)
	require.Equal(t, "dave", target.AuthID)
	require.Equal(t, "secret", target.Password)

	require.NoError(t, common.LoadStandaloneTarget(dir, common.StandaloneMachineRealm("keyed")))
	asks := common.StandaloneMachineRealm("asks")
	require.NoError(t, common.LoadStandaloneTarget(dir, asks))
	target, _ = common.StandaloneTargetFor(asks)
	_, err := target.Authenticator()
	require.ErrorContains(t, err, "no saved private key or password", "connecting fails, without asking")
	require.ErrorContains(t, common.LoadStandaloneTarget(dir, common.StandaloneMachineRealm("nope")),
		"no standalone machine named nope")
}
