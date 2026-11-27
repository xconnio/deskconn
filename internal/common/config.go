package common

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	auth "github.com/xconnio/xconn/bindings/go/auth"
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

// StandaloneMachine is a machine served without an account (desk serve --mode standalone),
// saved on this client with "desk machine add" to connect to directly at URL. It
// authenticates as Username with PrivateKey if set, otherwise with a password asked for on
// connecting.
type StandaloneMachine struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	CertHash   string `yaml:"cert_hash,omitempty"`
	PrivateKey string `yaml:"private_key,omitempty"`
	Password   string `yaml:"password,omitempty"` // when there's no PrivateKey
	Username   string `yaml:"username,omitempty"`
}

// User is the username machine logs in as: Username, or the current user's.
func (m StandaloneMachine) User() string {
	if m.Username != "" {
		return m.Username
	}
	return CurrentUsername()
}

// Target returns the target to connect to m with.
func (m StandaloneMachine) Target() *StandaloneTarget {
	return &StandaloneTarget{Name: m.Name, URL: m.URL, CertHash: m.CertHash, AuthID: m.User(),
		PrivateKey: m.PrivateKey, Password: m.Password}
}

type Config struct {
	Machines   []Machine           `yaml:"machines"`
	Printing   PrintingConfig      `yaml:"printing,omitempty"`
	Screenshot ScreenshotConfig    `yaml:"screenshot,omitempty"`
	Standalone []StandaloneMachine `yaml:"standalone,omitempty"`
}

// StandaloneTarget is a standalone machine the CLI connects to directly, instead of a
// machine from the cloud account. It authenticates with PrivateKey if set, otherwise as user
// AuthID with Password: desk machine add saves one of them, so connecting never asks.
type StandaloneTarget struct {
	Name       string // what it's saved as in config.yml
	URL        string // quic://host:port, or https://host:port[/path] for WebTransport
	CertHash   string // SHA-256 fingerprint to pin the machine's certificate to, if set
	AuthID     string
	PrivateKey string // hex ed25519 seed
	Password   string
}

// Authenticator returns a new client authenticator for target: cryptosign with its private
// key, or wampcra with its password.
func (t *StandaloneTarget) Authenticator() (auth.ClientAuthenticator, error) {
	switch {
	case t.PrivateKey != "":
		authenticator, err := auth.NewCryptoSignAuthenticator(t.AuthID, t.PrivateKey, map[string]any{})
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %w", err)
		}
		return authenticator, nil
	case t.Password != "":
		return auth.NewWAMPCRAAuthenticator(t.AuthID, t.Password, map[string]any{}), nil
	default:
		return nil, fmt.Errorf("standalone machine %s has no saved private key or password; add it again with "+
			"one: desk machine remove %s, then desk machine add %s --url %s", t.Name, t.Name, t.Name, t.URL)
	}
}

// standaloneTargets are the standalone machines this process connects to, by the realm the
// CLI names each by (StandaloneMachineRealm).
var standaloneTargets = struct { //nolint:gochecknoglobals // set from the CLI's flags or desk top
	sync.Mutex
	byRealm map[string]*StandaloneTarget
}{byRealm: map[string]*StandaloneTarget{}}

// SetStandaloneTargetFor makes realm (a StandaloneMachineRealm) resolve to target for this
// process.
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

// IsStandaloneMachineRealm reports whether realm is a StandaloneMachineRealm.
func IsStandaloneMachineRealm(realm string) bool {
	return strings.HasPrefix(realm, StandaloneRealm+"/")
}

// StandaloneMachineRealm is the realm the CLI names the standalone machine saved as name by,
// so that one process can use several (desk top). The machine itself serves StandaloneRealm,
// which connecting to it asks for instead (see ServedRealm).
func StandaloneMachineRealm(name string) string { return StandaloneRealm + "/" + name }

// ServedRealm is the realm realm's machine serves: StandaloneRealm for a StandaloneMachineRealm,
// otherwise realm itself.
func ServedRealm(realm string) string {
	if IsStandaloneMachineRealm(realm) {
		return StandaloneRealm
	}
	return realm
}

var ErrKeyExpired = errors.New("authentication key expired, please login again")

func CredentialsFilePath() (string, error) {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDirectory, "credentials.json"), nil
}

// CfgDirectory returns ~/.deskconn, creating it if needed. It holds the machine's keys and the
// daemon's socket, so only its owner may use it.
func CfgDirectory() (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home dir: %w", err)
	}

	cfgDirectory := filepath.Join(homedir, ".deskconn")
	if err := os.MkdirAll(cfgDirectory, 0o700); err != nil {
		return "", err
	}
	return cfgDirectory, nil
}

func MachinesFromCfg(cfgDirectory string) ([]Machine, error) {
	config, err := readCfg(cfgDirectory)
	if err != nil {
		return []Machine{}, err
	}
	return config.Machines, nil
}

func readCfg(cfgDirectory string) (Config, error) {
	var config Config
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil {
		return config, err
	}
	err = yaml.Unmarshal(data, &config)
	return config, err
}

// LoadStandaloneTarget makes realm, a StandaloneMachineRealm, resolve to the standalone
// machine config.yml saves under its name, read afresh: for the local daemon, which only
// knows of standalone machines from there.
func LoadStandaloneTarget(cfgDirectory, realm string) error {
	name := strings.TrimPrefix(realm, StandaloneRealm+"/")
	config, err := readCfg(cfgDirectory)
	if err != nil {
		return fmt.Errorf("read standalone machine %s: %w", name, err)
	}
	for _, machine := range config.Standalone {
		if machine.Name != name {
			continue
		}
		SetStandaloneTargetFor(realm, machine.Target())
		return nil
	}
	return fmt.Errorf("no standalone machine named %s", name)
}

func ReadCredentials(cfgDirectory string) (string, string, error) {
	path := filepath.Join(cfgDirectory, "id_ed25519")

	credentialsStr, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("user not logged in, run `desk auth login` first")
		}
		return "", "", err
	}

	credentials := strings.Split(string(credentialsStr), " ")
	if len(credentials) < 2 {
		return "", "", fmt.Errorf("malformed credentials file: %s", path)
	}
	privKey := strings.TrimSpace(credentials[0])
	authid := strings.TrimSpace(credentials[1])

	if len(credentials) >= 3 {
		expiresAtStr := strings.TrimSpace(credentials[2])
		expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtStr)
		if err == nil && time.Now().After(expiresAt) {
			return "", "", fmt.Errorf("%w", ErrKeyExpired)
		}
	}

	return authid, privKey, nil
}

type Credentials struct {
	Realm      string `json:"realm"`
	AuthID     string `json:"authid"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"` // #nosec
}

// CurrentUsername is the username a standalone machine logs in as unless --username says otherwise.
func CurrentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "deskconn"
}
