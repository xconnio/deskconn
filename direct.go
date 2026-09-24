package deskconn

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	xconnauth "github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const (
	// DirectRealmPrefix marks the realm of a device reached directly (standalone
	// xlink) instead of through the cloud. The part after it is the device name;
	// it is only a local lookup key, the device itself serves StandaloneRealm.
	DirectRealmPrefix = "direct."

	// DefaultStandalonePort is standalone xlink's default QUIC (UDP) port.
	DefaultStandalonePort = "18080"
)

func IsDirectRealm(realm string) bool {
	return strings.HasPrefix(realm, DirectRealmPrefix)
}

// CertFingerprint is the identity clients pin for a standalone device's certificate.
func CertFingerprint(certDER []byte) string {
	sum := sha256.Sum256(certDER)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EnsureDirectKey returns the key this machine authenticates to direct devices with,
// creating it on first use. Unlike the cloud login key it never expires and survives
// logout. The file holds "<private key hex> <authid>"; authid defaults to the OS user.
func EnsureDirectKey(cfgDirectory string) (authid, publicKey, privateKey string, err error) {
	path := filepath.Join(cfgDirectory, "direct_ed25519")

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, privateKey, err = xconnauth.GenerateCryptoSignKeyPair()
		if err != nil {
			return "", "", "", err
		}
		authid = "deskconn"
		if u, err := user.Current(); err == nil && u.Username != "" {
			authid = u.Username
		}
		data = []byte(privateKey + " " + authid + "\n")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return "", "", "", err
		}
	} else if err != nil {
		return "", "", "", err
	}

	privateKey, authid, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	seed, err := hex.DecodeString(privateKey)
	if !ok || err != nil || len(seed) != ed25519.SeedSize {
		return "", "", "", fmt.Errorf("malformed key file: %s", path)
	}
	publicKey = hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))

	return authid, publicKey, privateKey, nil
}

// clientCredentials returns the authid and private key this machine uses on realm's
// device: its direct key for direct devices, its cloud login otherwise.
func clientCredentials(realm, cfgDirectory string) (string, string, error) {
	if IsDirectRealm(realm) {
		authid, _, privateKey, err := EnsureDirectKey(cfgDirectory)
		return authid, privateKey, err
	}
	return ReadCredentials(cfgDirectory)
}

// NormalizeDirectAddress adds the default port to an address given without one.
func NormalizeDirectAddress(address string) string {
	if _, _, err := net.SplitHostPort(address); err != nil {
		return net.JoinHostPort(address, DefaultStandalonePort)
	}
	return address
}

func directDeviceByRealm(cfgDirectory, realm string) (Device, error) {
	devices, err := DevicesFromCfg(cfgDirectory)
	if err != nil {
		return Device{}, err
	}
	for _, d := range devices {
		if d.Realm == realm && d.Address != "" {
			return d, nil
		}
	}
	return Device{}, fmt.Errorf("direct device for realm %s not found", realm)
}

// ConnectDirectQUIC connects to a standalone device at its address, accepting only
// the certificate whose fingerprint was pinned when the device was added.
func ConnectDirectQUIC(ctx context.Context, device Device, cfgDirectory string) (*xconn.QUICSession, error) {
	authid, _, privateKey, err := EnsureDirectKey(cfgDirectory)
	if err != nil {
		return nil, err
	}
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privateKey, nil)
	if err != nil {
		return nil, err
	}

	return xconn.ConnectQUIC(ctx, device.Address, StandaloneRealm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     pinnedTLSConfig(device.Fingerprint),
	})
}

func pinnedTLSConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		// Chain verification is replaced by the fingerprint check below: standalone
		// devices use a self-signed certificate.
		InsecureSkipVerify: true, //nolint:gosec
		// VerifyConnection, unlike VerifyPeerCertificate, also runs on resumed sessions.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("device presented no certificate")
			}
			if got := CertFingerprint(state.PeerCertificates[0].Raw); !strings.EqualFold(got, fingerprint) {
				return fmt.Errorf("device certificate fingerprint %s does not match pinned %s", got, fingerprint)
			}
			return nil
		},
	}
}

// AddDirectDevice saves device to config.yml under a name no other device uses.
func AddDirectDevice(cfgDirectory string, device Device) error {
	return updateDevices(cfgDirectory, func(devices []Device) ([]Device, error) {
		for _, d := range devices {
			if d.Name == device.Name || d.Alias == device.Name {
				return nil, fmt.Errorf("a device named %q already exists", device.Name)
			}
		}
		return append(devices, device), nil
	})
}

// RemoveDirectDevice removes the direct device with the given name or alias.
func RemoveDirectDevice(cfgDirectory, name string) error {
	return updateDevices(cfgDirectory, func(devices []Device) ([]Device, error) {
		for i, d := range devices {
			if d.Address != "" && (d.Name == name || d.Alias == name) {
				return append(devices[:i], devices[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("direct device %q not found", name)
	})
}
