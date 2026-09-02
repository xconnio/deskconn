//go:build linux

package iptun

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// OpenTUN creates (or reopens) a Linux TUN interface named name and returns
// a *os.File for reading/writing raw IP packets through it, one packet per
// Read/Write.
func OpenTUN(name string) (tun *os.File, ifaceName string, err error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w", err)
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("build ifreq for %q: %w", name, err)
	}
	// IFF_NO_PI: don't prefix packets with the 4-byte tun_pi header -- we
	// only ever carry IPv4, so there's nothing useful for it to tell us.
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)

	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("TUNSETIFF %q: %w", name, err)
	}

	return os.NewFile(uintptr(fd), "/dev/net/tun"), ifr.Name(), nil
}

// ConfigureTUNAddress assigns cidr (e.g. "10.66.0.2/24") to iface, sets its
// MTU, and brings the link up.
func ConfigureTUNAddress(iface, cidr string, mtu int) error {
	if err := runNetCmd("ip", "addr", "add", cidr, "dev", iface); err != nil {
		return fmt.Errorf("assign %s to %s: %w", cidr, iface, err)
	}
	if err := runNetCmd("ip", "link", "set", "dev", iface, "mtu", strconv.Itoa(mtu)); err != nil {
		return fmt.Errorf("set %s mtu: %w", iface, err)
	}
	if err := runNetCmd("ip", "link", "set", "dev", iface, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", iface, err)
	}
	return nil
}
