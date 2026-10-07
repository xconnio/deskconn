package common_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
)

func TestLoadOrCreateCertificateKeepsCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	first, err := common.LoadOrCreateCertificate(certFile, keyFile)
	require.NoError(t, err)
	second, err := common.LoadOrCreateCertificate(certFile, keyFile)
	require.NoError(t, err)
	require.Equal(t, common.CertificateFingerprint(first), common.CertificateFingerprint(second))
	require.Len(t, common.CertificateFingerprint(first), 64)
}

func TestPinnedTLSConfigValidatesFingerprint(t *testing.T) {
	_, err := common.PinnedTLSConfig("not-hex")
	require.Error(t, err)
	_, err = common.PinnedTLSConfig("AB:CD")
	require.Error(t, err)

	dir := t.TempDir()
	cert, err := common.LoadOrCreateCertificate(filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"))
	require.NoError(t, err)
	_, err = common.PinnedTLSConfig(common.CertificateFingerprint(cert))
	require.NoError(t, err)
}
