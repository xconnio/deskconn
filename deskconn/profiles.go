package deskconn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// profilesFile keeps the profiles apart from config.yml: they hold private keys, and other
// processes rewrite config.yml with their own view of it.
const profilesFile = "profiles.yml"

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

// ErrProfileNotFound is returned for a profile name that isn't saved.
var ErrProfileNotFound = errors.New("profile not found")

// Profiles returns the saved profiles, sorted by name; none if there's no profiles file.
func Profiles(cfgDirectory string) ([]Profile, error) {
	data, err := os.ReadFile(filepath.Join(cfgDirectory, profilesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var profiles []Profile
	if err := yaml.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", profilesFile, err)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// FindProfile returns the profile saved as name.
func FindProfile(cfgDirectory, name string) (Profile, error) {
	profiles, err := Profiles(cfgDirectory)
	if err != nil {
		return Profile{}, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	return Profile{}, fmt.Errorf("%w: %s", ErrProfileNotFound, name)
}

// AddProfile saves profile, refusing a name that's already taken.
func AddProfile(cfgDirectory string, profile Profile) error {
	profiles, err := Profiles(cfgDirectory)
	if err != nil {
		return err
	}
	for _, existing := range profiles {
		if existing.Name == profile.Name {
			return fmt.Errorf("profile %s already exists; remove it first", profile.Name)
		}
	}
	return saveProfiles(cfgDirectory, append(profiles, profile))
}

// RemoveProfile deletes the profile saved as name.
func RemoveProfile(cfgDirectory, name string) error {
	profiles, err := Profiles(cfgDirectory)
	if err != nil {
		return err
	}
	for i, profile := range profiles {
		if profile.Name == name {
			return saveProfiles(cfgDirectory, append(profiles[:i], profiles[i+1:]...))
		}
	}
	return fmt.Errorf("%w: %s", ErrProfileNotFound, name)
}

func saveProfiles(cfgDirectory string, profiles []Profile) error {
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	// Saving the key is the point, like ~/.deskconn/id_ed25519; the file is owner only.
	data, err := yaml.Marshal(profiles) //nolint:gosec
	if err != nil {
		return err
	}
	// Owner only: profiles hold private keys.
	return os.WriteFile(filepath.Join(cfgDirectory, profilesFile), data, 0600)
}
