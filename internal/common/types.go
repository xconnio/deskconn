package common

import (
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/xconnio/wampproto-go/auth"
	xconnauth "github.com/xconnio/xconn-go/auth"
)

type Organization struct {
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name" yaml:"name"`
}

type Machine struct {
	Authid       string       `json:"authid" yaml:"authid"`
	ID           string       `json:"id" yaml:"id"`
	Name         string       `json:"name" yaml:"name"`
	Organization Organization `json:"organization" yaml:"organization"`
	Realm        string       `json:"realm" yaml:"realm"`
	Alias        string       `yaml:"alias"`
	Connected    bool         `yaml:"-" json:"-"`
}

// PrintMode is remotecontrol-app's printing setting, kept in config.yml.
type PrintMode string

type PrintingConfig struct {
	Mode PrintMode `yaml:"mode,omitempty"`
}

type ScreenshotConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// Profile is a standalone machine saved with "desk profile add", to connect to directly
// with "desk --profile <name>". It authenticates with PrivateKey if set, otherwise as
// AuthID with a password asked for on each use.
type Profile struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	CertHash   string `yaml:"cert_hash,omitempty"`
	PrivateKey string `yaml:"private_key,omitempty"`
	AuthID     string `yaml:"authid,omitempty"`
}

type Config struct {
	Machines   []Machine        `yaml:"machines"`
	Printing   PrintingConfig   `yaml:"printing,omitempty"`
	Screenshot ScreenshotConfig `yaml:"screenshot,omitempty"`
	Profiles   []Profile        `yaml:"profiles,omitempty"`
}

// UnmarshalYAML also accepts the "devices" key that config.yml used before machines were
// renamed, so existing configs keep working; the next write saves them under "machines".
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type plain Config
	var raw struct {
		plain          `yaml:",inline"`
		LegacyMachines []Machine `yaml:"devices"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*c = Config(raw.plain)
	if len(c.Machines) == 0 {
		c.Machines = raw.LegacyMachines
	}
	return nil
}

// StandaloneTarget is a standalone machine the CLI connects to directly (desk --profile),
// instead of a machine from the cloud account. It authenticates with PrivateKey if set,
// otherwise as user AuthID with Password, asking ReadPassword for it on first use if empty.
type StandaloneTarget struct {
	URL          string // quic://host:port, or https://host:port[/path] for WebTransport
	CertHash     string // SHA-256 fingerprint to pin the machine's certificate to, if set
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

// SetPassword sets the password target connects with, for one asked for other than by
// ReadPassword (desk top), or "" to forget a wrong one.
func (t *StandaloneTarget) SetPassword(password string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Password = password
}

// NeedsPassword reports whether target connects with a password it doesn't have yet.
func (t *StandaloneTarget) NeedsPassword() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.PrivateKey == "" && t.Password == ""
}

// standaloneTargets are the standalone machines this process connects to, by the realm the
// CLI names each by: StandaloneRealm for desk --profile, ProfileRealm for each of desk top's.
var standaloneTargets = struct { //nolint:gochecknoglobals // set from the CLI's flags or desk top
	sync.Mutex
	byRealm map[string]*StandaloneTarget
}{byRealm: map[string]*StandaloneTarget{}}

// SetStandaloneTarget makes StandaloneRealm resolve to target for this process.
func SetStandaloneTarget(target *StandaloneTarget) { SetStandaloneTargetFor(StandaloneRealm, target) }

// SetStandaloneTargetFor makes realm (StandaloneRealm or a ProfileRealm) resolve to target
// for this process.
func SetStandaloneTargetFor(realm string, target *StandaloneTarget) {
	standaloneTargets.Lock()
	defer standaloneTargets.Unlock()
	standaloneTargets.byRealm[realm] = target
}

// StandaloneTargetFor returns the standalone target if realm refers to one.
func StandaloneTargetFor(realm string) (*StandaloneTarget, bool) {
	standaloneTargets.Lock()
	defer standaloneTargets.Unlock()
	target, ok := standaloneTargets.byRealm[realm]
	return target, ok && target != nil
}

// ProfileRealm is the realm the CLI names profile's standalone machine by, so that one process
// can use several (desk top). The machine itself serves StandaloneRealm, which connecting
// to it asks for instead (see ServedRealm).
func ProfileRealm(profile string) string { return StandaloneRealm + "/" + profile }

// ServedRealm is the realm realm's machine serves: StandaloneRealm for a ProfileRealm,
// otherwise realm itself.
func ServedRealm(realm string) string {
	if strings.HasPrefix(realm, StandaloneRealm+"/") {
		return StandaloneRealm
	}
	return realm
}

// StreamProxyRequest is what the CLI sends on userd's stream proxy socket. With an
// empty Kind it only asks which transport the persistent connection to Realm's machine
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
