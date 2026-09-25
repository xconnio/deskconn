package common

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

	// Address and Fingerprint are set for direct (standalone) devices only.
	Address     string `yaml:"address,omitempty" json:"-"`
	Fingerprint string `yaml:"fingerprint,omitempty" json:"-"`
}

type PrintingConfig struct {
	Mode PrintMode `yaml:"mode,omitempty"`
}

type ScreenshotConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// StandaloneConfig runs xlink without the cloud: it serves the device realm
// directly over QUIC to the listed keys.
type StandaloneConfig struct {
	Enabled    bool                  `yaml:"enabled,omitempty"`
	Listen     string                `yaml:"listen,omitempty"`
	Principals []StandalonePrincipal `yaml:"principals,omitempty"`
}

type StandalonePrincipal struct {
	AuthID         string   `yaml:"authid"`
	AuthorizedKeys []string `yaml:"authorized_keys"`
}

type Config struct {
	Devices    []Device         `yaml:"devices"`
	Printing   PrintingConfig   `yaml:"printing,omitempty"`
	Screenshot ScreenshotConfig `yaml:"screenshot,omitempty"`
	Standalone StandaloneConfig `yaml:"standalone,omitempty"`
}
