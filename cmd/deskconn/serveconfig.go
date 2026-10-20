package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// serveConfig is the --config file of desk serve: the mode, and for standalone mode where
// to listen and who may connect.
type serveConfig struct {
	Mode   string            `yaml:"mode"`
	Listen serveConfigListen `yaml:"listen"`
	Users  []serveConfigUser `yaml:"users"`
}

// serveConfigListen is where standalone mode serves: QUIC, WebTransport or both, by which
// addresses are set (QUIC on its default address if neither is).
type serveConfigListen struct {
	QUIC         string `yaml:"quic"`
	WebTransport string `yaml:"webtransport"`
	TLSCert      string `yaml:"tls-cert"`
	TLSKey       string `yaml:"tls-key"`
}

// serveConfigUser may connect in standalone mode with its password, any of its authorized
// keys, or either. A key is let in whatever username its holder presents: username only labels it.
type serveConfigUser struct {
	Username       string   `yaml:"username"`
	Password       string   `yaml:"password"`
	AuthorizedKeys []string `yaml:"authorized-keys"`
}

// transport is the standalone transport the listen addresses set, or "" if they set none.
func (listen serveConfigListen) transport() string {
	switch {
	case listen.QUIC != "" && listen.WebTransport != "":
		return transportBoth
	case listen.WebTransport != "":
		return transportWebTransport
	case listen.QUIC != "":
		return transportQUIC
	default:
		return ""
	}
}

// loadServeConfig reads and checks the --config file at path. Relative tls-cert and
// tls-key paths in it are taken relative to the file.
func loadServeConfig(path string) (*serveConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var config serveConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := config.check(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	for _, file := range []*string{&config.Listen.TLSCert, &config.Listen.TLSKey} {
		if *file != "" && !filepath.IsAbs(*file) {
			*file = filepath.Join(dir, *file)
		}
	}

	if slices.ContainsFunc(config.Users, func(user serveConfigUser) bool { return user.Password != "" }) {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			log.Warnf("config %s has passwords but other users can read it (chmod 600 it)", path)
		}
	}
	return &config, nil
}

func (config *serveConfig) check() error {
	switch config.Mode {
	case "", serveModeUser, serveModeStandalone:
	case serveModeRoot:
		return errors.New("mode root is started by desk vpn, not from a config")
	default:
		return fmt.Errorf("invalid mode %q: want %s or %s", config.Mode, serveModeUser, serveModeStandalone)
	}

	if (config.Listen.TLSCert == "") != (config.Listen.TLSKey == "") {
		return errors.New("listen: tls-cert and tls-key go together")
	}

	return checkUsers(config.Users)
}

// checkUsers checks who may connect in standalone mode, from a config or --user.
func checkUsers(users []serveConfigUser) error {
	usernames := make(map[string]bool, len(users))
	keys := map[string]string{}
	for i, user := range users {
		switch {
		case user.Username == "":
			return fmt.Errorf("user %d: needs a username", i+1)
		case strings.Contains(user.Username, ":"):
			return fmt.Errorf("user %q: a username can't contain ':'", user.Username)
		case usernames[user.Username]:
			return fmt.Errorf("user %q: listed twice", user.Username)
		case user.Password == "" && len(user.AuthorizedKeys) == 0:
			return fmt.Errorf("user %q: needs a password or authorized-keys", user.Username)
		}
		for _, key := range user.AuthorizedKeys {
			if !isPublicKey(key) {
				return fmt.Errorf("user %q: key %q isn't a public key (%d hex digits, see desk keygen)",
					user.Username, key, 2*ed25519.PublicKeySize)
			}
			// Since a key logs in whatever username its holder presents, one listed for two users
			// would let either one in as the other.
			if owner, ok := keys[strings.ToLower(key)]; ok {
				return fmt.Errorf("user %q: key %s is already %q's", user.Username, key, owner)
			}
			keys[strings.ToLower(key)] = user.Username
		}
		usernames[user.Username] = true
	}
	return nil
}

// isPublicKey reports whether s is a public key as desk keygen prints it: 64 hex digits.
func isPublicKey(s string) bool {
	raw, err := hex.DecodeString(s)
	return err == nil && len(raw) == ed25519.PublicKeySize
}

// sampleServeConfig is what desk serve init writes: every setting, with its default.
const sampleServeConfig = `# Config for "desk serve --config <this file>". Flags given as well override it, and
# --public-key/--user add to its users.

# user: serve this machine through the cloud, to your account.
# standalone: serve it directly to the users below, without the cloud.
mode: standalone

# Where standalone mode serves: over QUIC, WebTransport or both, by which addresses are
# set (QUIC on 0.0.0.0:18080 if neither is).
listen:
  quic: 0.0.0.0:18080
  # webtransport: 0.0.0.0:18081

  # TLS certificate and key (PEM); relative paths are relative to this file. Without them,
  # a self-signed certificate is generated and kept in ~/.deskconn.
  # tls-cert: cert.pem
  # tls-key: key.pem

# Who may connect in standalone mode: with a password, or with the private key matching
# any of a user's public keys (make a pair with "desk keygen"). A key is let in whatever
# username its holder presents.
users:
  # - username: alice
  #   password: change-me
  # - username: bob
  #   authorized-keys:
  #     - 65160c38...
`

// writeSampleServeConfig writes sampleServeConfig to path, readable only by its owner since
// it's where passwords go. It refuses to replace an existing file unless force is set.
func writeSampleServeConfig(path string, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists (--force to overwrite it)", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(sampleServeConfig), 0o600); err != nil {
		return err
	}
	// WriteFile keeps the mode of a file it overwrites.
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	// The full path, so the command works from anywhere (e.g. in the service's ExecStart).
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	fmt.Printf("Wrote a sample config to %s. Add your public keys or users to it, then run:\n"+
		"  desk serve --config %s\n", path, path)
	return nil
}
