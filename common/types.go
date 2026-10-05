package common

import (
	"fmt"
	"sync"

	"github.com/xconnio/wampproto-go/auth"
	xconnauth "github.com/xconnio/xconn-go/auth"
)

type Organization struct {
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name" yaml:"name"`
}

type Device struct {
	Authid       string       `json:"authid" yaml:"authid"`
	ID           string       `json:"id" yaml:"id"`
	Name         string       `json:"name" yaml:"name"`
	Organization Organization `json:"organization" yaml:"organization"`
	Realm        string       `json:"realm" yaml:"realm"`
	Alias        string       `yaml:"alias"`
	Connected    bool         `yaml:"-" json:"-"`
}

type PrintingConfig struct {
	Mode PrintMode `yaml:"mode,omitempty"`
}

type ScreenshotConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

type Config struct {
	Devices    []Device         `yaml:"devices"`
	Printing   PrintingConfig   `yaml:"printing,omitempty"`
	Screenshot ScreenshotConfig `yaml:"screenshot,omitempty"`
}

// StandaloneTarget is a standalone device the CLI connects to directly (desk --url),
// instead of a device from the cloud account. It authenticates with PrivateKey if set,
// otherwise as user AuthID with Password, asking ReadPassword for it on first use if empty.
type StandaloneTarget struct {
	URL          string // tcp://host:port or unix:///path
	AuthID       string
	PrivateKey   string // hex ed25519 seed
	Password     string
	ReadPassword func() (string, error)

	mu sync.Mutex
}

// Authenticator returns a new client authenticator for target: cryptosign with its private
// key, or wampcra with its password.
func (t *StandaloneTarget) Authenticator() (auth.ClientAuthenticator, error) {
	if t.PrivateKey == "" {
		password, err := t.password()
		if err != nil {
			return nil, err
		}
		return xconnauth.NewWAMPCRAAuthenticator(t.AuthID, password, map[string]any{}), nil
	}
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(t.AuthID, t.PrivateKey, map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	return authenticator, nil
}

// password returns target's password, reading it with ReadPassword the first time it's
// needed, so commands that never connect don't ask for it.
func (t *StandaloneTarget) password() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Password == "" && t.ReadPassword != nil {
		password, err := t.ReadPassword()
		if err != nil {
			return "", err
		}
		t.Password = password
	}
	return t.Password, nil
}

var standaloneTarget *StandaloneTarget //nolint:gochecknoglobals // set once from the CLI's flags

// SetStandaloneTarget makes StandaloneRealm resolve to target for this process.
func SetStandaloneTarget(target *StandaloneTarget) { standaloneTarget = target }

// StandaloneTargetFor returns the standalone target if realm refers to it.
func StandaloneTargetFor(realm string) (*StandaloneTarget, bool) {
	if standaloneTarget == nil || realm != StandaloneRealm {
		return nil, false
	}
	return standaloneTarget, true
}

// StreamProxyRequest is what the CLI sends on deskconnd's stream proxy socket. With an
// empty Kind it only asks which transport the persistent connection to Realm's device
// has; otherwise it opens a raw QUIC stream, or a data channel named Label, on it.
type StreamProxyRequest struct {
	Realm string    `json:"realm"`
	Kind  RelayKind `json:"kind,omitempty"`
	Label string    `json:"label,omitempty"`
}

// StreamProxyResponse answers a StreamProxyRequest. After a successful open, the rest of
// the connection is the stream itself.
type StreamProxyResponse struct {
	Kind  RelayKind `json:"kind,omitempty"`
	Error string    `json:"error,omitempty"`
}
