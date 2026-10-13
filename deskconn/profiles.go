package deskconn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
)

// ErrProfileNotFound is returned for a profile name that isn't saved.
var ErrProfileNotFound = errors.New("profile not found")

// Profiles returns the profiles saved in config.yml, sorted by name.
func Profiles(cfgDirectory string) ([]common.Profile, error) {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return nil, err
	}
	return config.Profiles, nil
}

// FindProfile returns the profile saved as name.
func FindProfile(cfgDirectory, name string) (common.Profile, error) {
	profiles, err := Profiles(cfgDirectory)
	if err != nil {
		return common.Profile{}, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	return common.Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, name)
}

// AddProfile saves profile, refusing a name that's already taken.
func AddProfile(cfgDirectory string, profile common.Profile) error {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return err
	}
	for _, existing := range config.Profiles {
		if existing.Name == profile.Name {
			return fmt.Errorf("profile %s already exists; remove it first", profile.Name)
		}
	}
	config.Profiles = append(config.Profiles, profile)
	return writeConfig(cfgDirectory, config)
}

// RemoveProfile deletes the profile saved as name.
func RemoveProfile(cfgDirectory, name string) error {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return err
	}
	for i, profile := range config.Profiles {
		if profile.Name == name {
			config.Profiles = append(config.Profiles[:i], config.Profiles[i+1:]...)
			return writeConfig(cfgDirectory, config)
		}
	}
	return fmt.Errorf("%w: %s", ErrProfileNotFound, name)
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

// writeConfig saves config.yml, with its profiles sorted by name.
func writeConfig(cfgDirectory string, config common.Config) error {
	sort.Slice(config.Profiles, func(i, j int) bool { return config.Profiles[i].Name < config.Profiles[j].Name })
	// Profiles hold private keys, like ~/.deskconn/id_ed25519; config.yml is owner only.
	data, err := yaml.Marshal(config) //nolint:gosec
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), data, 0600)
}
