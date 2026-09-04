package info

import (
	"strings"

	psnet "github.com/shirou/gopsutil/net"
)

// virtualInterfaceKeywords match common non-hardware adapters Windows reports alongside real
// NICs: VPN/tunnel adapters, virtualization host-only/NAT adapters, and Bluetooth PAN.
var virtualInterfaceKeywords = []string{ //nolint: gochecknoglobals
	"loopback", "virtual", "vpn", "tap", "tunnel", "teredo", "isatap",
	"hyper-v", "vmware", "virtualbox", "bluetooth", "wan miniport",
}

// isPhysicalInterface reports whether iface looks like a real hardware device. Windows has no
// sysfs equivalent, so this relies on two conventions in the adapter's friendly name: a "*"
// marks it as hidden/pseudo (Microsoft's own convention for non-physical devices, e.g.
// "Local Area Connection* 1"), and known virtual/tunnel/VM adapter vendors are name-matched.
// Imperfect (some real-hardware names may slip through unmatched), but far better than the
// previous Linux-only check, which excluded every interface unconditionally on Windows.
func isPhysicalInterface(iface psnet.InterfaceStat) bool {
	if strings.Contains(iface.Name, "*") {
		return false
	}
	lower := strings.ToLower(iface.Name)
	for _, kw := range virtualInterfaceKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}
