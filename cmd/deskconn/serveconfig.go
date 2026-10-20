package main

import (
	"bytes"
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

// serveConfig is the --config file of desk serve: the mode, and the standalone settings
// its flags otherwise set.
type serveConfig struct {
	Mode       string                `yaml:"mode"`
	Standalone standaloneServeConfig `yaml:"standalone"`
}

type standaloneServeConfig struct {
	Transport           string            `yaml:"transport"`
	QUICAddress         string            `yaml:"quic-address"`
	WebTransportAddress string            `yaml:"webtransport-address"`
	TLSCert             string            `yaml:"tls-cert"`
	TLSKey              string            `yaml:"tls-key"`
	PublicKeys          []string          `yaml:"public-keys"`
	Users               []serveConfigUser `yaml:"users"`
}

type serveConfigUser struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
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
	for _, file := range []*string{&config.Standalone.TLSCert, &config.Standalone.TLSKey} {
		if *file != "" && !filepath.IsAbs(*file) {
			*file = filepath.Join(dir, *file)
		}
	}

	if len(config.Standalone.Users) > 0 {
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

	transports := []string{transportQUIC, transportWebTransport, transportBoth}
	if t := config.Standalone.Transport; t != "" && !slices.Contains(transports, t) {
		return fmt.Errorf("invalid standalone transport %q: want %s", t, strings.Join(transports, ", "))
	}
	for i, user := range config.Standalone.Users {
		if user.Username == "" || user.Password == "" {
			return fmt.Errorf("standalone user %d: needs a username and a password", i+1)
		}
		if strings.Contains(user.Username, ":") {
			return fmt.Errorf("standalone user %q: a username can't contain ':'", user.Username)
		}
	}
	return nil
}

// sampleServeConfig is what desk serve init writes: every setting, with its default.
const sampleServeConfig = `# Config for "desk serve --config <this file>". Flags given as well override it, and
# --public-key/--user add to its keys and users.

# user: serve this machine through the cloud, to your account.
# standalone: serve it directly to the public keys and users below, without the cloud.
mode: standalone

# Used in standalone mode only.
standalone:
  # quic, webtransport or both.
  transport: quic
  quic-address: 0.0.0.0:18080
  webtransport-address: 0.0.0.0:18081

  # TLS certificate and key (PEM); relative paths are relative to this file. Without them,
  # a self-signed certificate is generated and kept in ~/.deskconn.
  # tls-cert: cert.pem
  # tls-key: key.pem

  # Public keys (hex) allowed to connect; make a key pair with "desk keygen".
  public-keys:
    # - 65160c38...

  # Accounts allowed to connect with a password.
  users:
    # - username: alice
    #   password: change-me
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
