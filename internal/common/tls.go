package common

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// localhostName is always among a generated certificate's names.
const localhostName = "localhost"

// selfSignedValidity is how long a generated self-signed certificate is valid. Clients pin
// its fingerprint (see PinnedTLSConfig), so it is long-lived rather than rotated.
const selfSignedValidity = 10 * 365 * 24 * time.Hour

// LoadOrCreateCertificate loads the PEM certificate and key in certFile and keyFile, first
// generating a self-signed certificate there if neither file exists. A generated
// certificate is kept, so its fingerprint stays the same across restarts.
func LoadOrCreateCertificate(certFile, keyFile string) (tls.Certificate, error) {
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		certPEM, keyPEM, err := GenerateSelfSignedCertificate()
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := os.MkdirAll(filepath.Dir(certFile), 0700); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
			return tls.Certificate{}, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load certificate: %w", err)
	}
	return cert, nil
}

// GenerateSelfSignedCertificate returns a new self-signed ECDSA P-256 certificate and its
// key, PEM encoded, for this host's name, localhost and the loopback addresses.
func GenerateSelfSignedCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	dnsNames := []string{localhostName}
	if host, err := os.Hostname(); err == nil && host != "" && host != localhostName {
		dnsNames = append(dnsNames, host)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[len(dnsNames)-1]},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(selfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// CertificateFingerprint returns the hex SHA-256 of cert's leaf certificate.
func CertificateFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return hex.EncodeToString(sum[:])
}

// normalizeFingerprint accepts a hex SHA-256 fingerprint in any case, with or without colons.
func normalizeFingerprint(fingerprint string) (string, error) {
	fp := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fingerprint), ":", ""))
	if b, err := hex.DecodeString(fp); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("invalid certificate fingerprint %q: want a hex SHA-256", fingerprint)
	}
	return fp, nil
}

// PinnedTLSConfig returns a client TLS config that accepts only a server certificate whose
// SHA-256 fingerprint (see CertificateFingerprint) is fingerprint, self-signed or not.
func PinnedTLSConfig(fingerprint string) (*tls.Config, error) {
	want, err := normalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		// Verification is replaced by the fingerprint check below.
		InsecureSkipVerify: true, //nolint:gosec
		// VerifyConnection, unlike VerifyPeerCertificate, also runs on resumed sessions.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			sum := sha256.Sum256(state.PeerCertificates[0].Raw)
			if got := hex.EncodeToString(sum[:]); got != want {
				return fmt.Errorf("server certificate fingerprint %s does not match the pinned %s", got, want)
			}
			return nil
		},
	}, nil
}
