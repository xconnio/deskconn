package deskconn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/internal/common"
)

// ErrStandaloneMachineNotFound is returned for a standalone machine name that isn't saved.
var ErrStandaloneMachineNotFound = errors.New("standalone machine not found")

// StandaloneMachines returns the standalone machines saved in config.yml, sorted by name.
func StandaloneMachines(cfgDirectory string) ([]common.StandaloneMachine, error) {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return nil, err
	}
	return config.Standalone, nil
}

// FindStandaloneMachine returns the standalone machine saved as name.
func FindStandaloneMachine(cfgDirectory, name string) (common.StandaloneMachine, error) {
	machines, err := StandaloneMachines(cfgDirectory)
	if err != nil {
		return common.StandaloneMachine{}, err
	}
	for _, machine := range machines {
		if machine.Name == name {
			return machine, nil
		}
	}
	return common.StandaloneMachine{}, fmt.Errorf("%w: %s", ErrStandaloneMachineNotFound, name)
}

// ManagedMachineNamed returns the machine of the account (as config.yml caches them) that
// name refers to exactly: by its name, alias or ID.
func ManagedMachineNamed(machines []common.Machine, name string) (common.Machine, bool) {
	for _, machine := range machines {
		if name != "" && (machine.Name == name || machine.Alias == name || machine.Authid == name) {
			return machine, true
		}
	}
	return common.Machine{}, false
}

// ManagedMachineConflict returns the machine of the account that name would also refer to,
// were it a standalone machine's: one that name refers to exactly (ManagedMachineNamed), or
// by the start of its ID.
func ManagedMachineConflict(machines []common.Machine, name string) (common.Machine, bool) {
	if machine, ok := ManagedMachineNamed(machines, name); ok {
		return machine, true
	}
	for _, machine := range machines {
		if name != "" && strings.HasPrefix(machine.Authid, name) {
			return machine, true
		}
	}
	return common.Machine{}, false
}

// AddStandaloneMachine saves machine, refusing a name that's already taken by a standalone
// machine or that refers to a machine of the account (see ManagedMachineConflict).
func AddStandaloneMachine(cfgDirectory string, machine common.StandaloneMachine) error {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return err
	}
	for _, existing := range config.Standalone {
		if existing.Name == machine.Name {
			return fmt.Errorf("a standalone machine named %q already exists; remove it first with "+
				"`desk machine remove %s`", machine.Name, machine.Name)
		}
	}
	if managed, ok := ManagedMachineConflict(config.Machines, machine.Name); ok {
		return fmt.Errorf("%q already refers to a managed machine of your account (%s, ID %s); choose another name",
			machine.Name, managed.Name, ShortID(managed.Authid))
	}
	config.Standalone = append(config.Standalone, machine)
	return writeConfig(cfgDirectory, config)
}

// RemoveStandaloneMachine deletes the standalone machine saved as name.
func RemoveStandaloneMachine(cfgDirectory, name string) error {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return err
	}
	for i, machine := range config.Standalone {
		if machine.Name == name {
			config.Standalone = append(config.Standalone[:i], config.Standalone[i+1:]...)
			return writeConfig(cfgDirectory, config)
		}
	}
	return fmt.Errorf("%w: %s", ErrStandaloneMachineNotFound, name)
}

// readConfig returns config.yml, empty if there's none yet.
func readConfig(cfgDirectory string) (common.Config, error) {
	var config common.Config
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("failed to parse config: %w", err)
	}
	return config, nil
}

// writeConfig saves config.yml, with its standalone machines sorted by name.
func writeConfig(cfgDirectory string, config common.Config) error {
	sort.Slice(config.Standalone, func(i, j int) bool { return config.Standalone[i].Name < config.Standalone[j].Name })
	// Standalone machines hold private keys, like ~/.deskconn/id_ed25519; config.yml is owner only.
	data, err := yaml.Marshal(config) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), data, 0600)
}

// ThroughDaemon reports whether realm's machine is reached through the local daemon's
// persistent connection by default: a managed machine's always, a standalone machine's while
// the daemon is running. Without the daemon, a standalone machine is reached on a connection
// of its own, as it was before the daemon could keep one: it needs neither daemon nor cloud.
func ThroughDaemon(cfgDirectory, realm string) bool {
	return !common.IsStandaloneMachineRealm(realm) || DaemonRunning(cfgDirectory)
}

// DefaultMode is the --mode to reach realm's machine with: mode if given, otherwise "" (the
// local daemon's persistent connection), or routed (a connection of its own) for a standalone
// machine while the daemon isn't running (see ThroughDaemon).
func DefaultMode(cfgDirectory, realm, mode string) string {
	if mode == "" && !ThroughDaemon(cfgDirectory, realm) {
		return ModeRouted
	}
	return mode
}

// DaemonRunning reports whether the current user's local daemon is listening on its socket:
// a socket left behind by one that's gone refuses the connection.
func DaemonRunning(cfgDirectory string) bool {
	conn, err := net.DialTimeout("unix", filepath.Join(cfgDirectory, "deskconn.sock"), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
