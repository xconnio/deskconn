package common_test

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

const (
	testUser     = "carol"
	testPassword = "s3cret"
)

// startStandalone serves the standalone realm over QUIC and WebTransport on local ports,
// with xconn's standalone authenticator and a self-signed certificate like deskconnd
// --standalone, and returns the QUIC and WebTransport URLs and the certificate fingerprint.
func startStandalone(t *testing.T, keys []string, passwords map[string]string) (quicURL, wtURL,
	certHash string) {
	t.Helper()
	router, err := xconnd.NewDeviceRouter(common.StandaloneRealm)
	require.NoError(t, err)
	t.Cleanup(router.Close)

	local, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
	require.NoError(t, err)
	resp := local.Register("io.xconn.test.echo", func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		return xconn.NewInvocationResult(inv.Args()...)
	}).Do()
	require.NoError(t, resp.Err)

	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	require.NoError(t, err)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	server := xconn.NewServer(router, xconnd.NewStandaloneAuthenticator(keys, passwords), &xconn.ServerConfig{})

	quicListener, err := server.ListenAndServeQUIC("127.0.0.1:0", tlsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { _ = quicListener.Close() })
	wtListener, err := server.ListenAndServeWebTransport("127.0.0.1:0", tlsConfig, xconnd.WebTransportPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = wtListener.Close() })
	go func() {
		for range quicListener.AcceptSession() { //nolint:revive // only draining
		}
	}()
	go func() {
		for range wtListener.AcceptSession() { //nolint:revive // only draining
		}
	}()

	return "quic://" + quicListener.Addr().String(), "https://" + wtListener.Addr().String(),
		common.CertificateFingerprint(cert)
}

func connectStandalone(t *testing.T, target *common.StandaloneTarget) (*common.DeviceConn, error) {
	t.Helper()
	authenticator, err := target.Authenticator()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return common.ConnectStandalone(ctx, target.URL, target.CertHash, common.StandaloneRealm, authenticator)
}

func TestParseStandaloneURL(t *testing.T) {
	u, err := common.ParseStandaloneURL("quic://203.0.113.5:18080")
	require.NoError(t, err)
	require.Equal(t, "quic://203.0.113.5:18080", u.String())

	u, err = common.ParseStandaloneURL("https://example.com:18081")
	require.NoError(t, err)
	require.Equal(t, "https://example.com:18081"+xconnd.WebTransportPath, u.String())

	u, err = common.ParseStandaloneURL("https://example.com:443/custom")
	require.NoError(t, err)
	require.Equal(t, "/custom", u.Path)

	for _, bad := range []string{"tcp://host:1", "unix:///tmp/d.sock", "quic://host", "https://host", "::"} {
		_, err := common.ParseStandaloneURL(bad)
		require.Error(t, err, bad)
	}
}

func TestStandaloneTransports(t *testing.T) {
	quicURL, wtURL, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword})
	for _, url := range []string{quicURL, wtURL} {
		conn, err := connectStandalone(t, &common.StandaloneTarget{URL: url, CertHash: certHash, AuthID: testUser,
			Password: testPassword})
		require.NoError(t, err, url)
		resp := conn.Call("io.xconn.test.echo").Args("hello").Do()
		require.NoError(t, resp.Err, url)
		require.Equal(t, "hello", resp.Args()[0])

		// Raw streams open alongside the session (the server side has no consumer, so
		// opening is all that can be checked here).
		stream, err := conn.OpenStream()
		require.NoError(t, err, url)
		_ = stream.Close()
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
	resp := conn.Call("io.xconn.test.echo").Args("hello").Do()
	require.NoError(t, resp.Err)
	require.Equal(t, "hello", resp.Args()[0])

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

func TestStandaloneTargetReadsPasswordOnce(t *testing.T) {
	url, _, certHash := startStandalone(t, nil, map[string]string{testUser: testPassword})
	reads := 0
	target := &common.StandaloneTarget{URL: url, CertHash: certHash,
		AuthID: testUser, ReadPassword: func() (string, error) {
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
