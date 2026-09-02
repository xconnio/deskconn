package info

import (
	"strings"

	psnet "github.com/shirou/gopsutil/net"
)

// virtualInterfacePrefixes are macOS's well-known non-hardware interface name prefixes: VPN
// tunnels (utun, ipsec, ppp), Apple Wireless Direct Link / AWDL-based features (awdl, llw, ap),
// bridges and virtualization adapters (bridge, vmnet, vnic), and other pseudo-devices (gif, stf).
var virtualInterfacePrefixes = []string{ //nolint: gochecknoglobals
	"lo", "utun", "ipsec", "ppp", "awdl", "llw", "ap", "bridge", "vmnet", "vnic", "gif", "stf",
}

// isPhysicalInterface reports whether iface looks like a real hardware device. macOS has no
// sysfs equivalent, so this relies on its interface naming convention: real NICs are named
// "enN" (Ethernet, WiFi, Thunderbolt), everything else recognized here is a known pseudo-device
// prefix. Imperfect (e.g. a Thunderbolt Bridge is also "enN" but isn't a NIC), but far better
// than the previous Linux-only check, which excluded every interface unconditionally on macOS.
func isPhysicalInterface(iface psnet.InterfaceStat) bool {
	name := strings.ToLower(iface.Name)
	for _, prefix := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}
