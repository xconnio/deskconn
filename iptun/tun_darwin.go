//go:build darwin

package iptun

import (
	"fmt"
	"os"
)

// OpenTUN is not yet implemented on darwin; macOS TUN devices (utun) use a
// different kernel control socket API than the Linux tuntap ioctls in
// tun_linux.go and need a separate implementation.
func OpenTUN(name string) (tun *os.File, ifaceName string, err error) {
	return nil, "", fmt.Errorf("iptun: OpenTUN is not supported on darwin yet")
}

// ConfigureTUNAddress is not yet implemented on darwin.
func ConfigureTUNAddress(iface, cidr string, mtu int) error {
	return fmt.Errorf("iptun: ConfigureTUNAddress is not supported on darwin yet")
}
