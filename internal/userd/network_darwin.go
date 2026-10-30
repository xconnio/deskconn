package userd

import (
	"strings"

	psnet "github.com/shirou/gopsutil/net"
)

// isPhysicalInterface reports whether iface is backed by a real hardware device. macOS has no
// /sys to check, but names every Ethernet and Wi-Fi device (built in, USB, Thunderbolt) enN,
// while virtual ones get other names (lo, bridge, utun, awdl, llw, anpi, gif, stf, ...).
func isPhysicalInterface(iface psnet.InterfaceStat) bool {
	return strings.HasPrefix(iface.Name, "en")
}
