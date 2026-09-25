package deskconn

import (
	"fmt"
	"net"

	"github.com/xconnio/deskconn/common"
)

const (

	// DefaultStandalonePort is standalone xlink's default QUIC (UDP) port.
	DefaultStandalonePort = "18080"
)

// clientCredentials returns the authid and private key this machine uses on realm's
// device: its direct key for direct devices, its cloud login otherwise.
func clientCredentials(realm, cfgDirectory string) (string, string, error) {
	if common.IsDirectRealm(realm) {
		authid, _, privateKey, err := common.EnsureDirectKey(cfgDirectory)
		return authid, privateKey, err
	}
	return common.ReadCredentials(cfgDirectory)
}

// NormalizeDirectAddress adds the default port to an address given without one.
func NormalizeDirectAddress(address string) string {
	if _, _, err := net.SplitHostPort(address); err != nil {
		return net.JoinHostPort(address, DefaultStandalonePort)
	}
	return address
}

// AddDirectDevice saves device to config.yml under a name no other device uses.
func AddDirectDevice(cfgDirectory string, device common.Device) error {
	return updateDevices(cfgDirectory, func(devices []common.Device) ([]common.Device, error) {
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
	return updateDevices(cfgDirectory, func(devices []common.Device) ([]common.Device, error) {
		for i, d := range devices {
			if d.Address != "" && (d.Name == name || d.Alias == name) {
				return append(devices[:i], devices[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("direct device %q not found", name)
	})
}
