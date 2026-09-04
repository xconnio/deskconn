package info

import (
	"os"

	psnet "github.com/shirou/gopsutil/net"
)

// isPhysicalInterface reports whether iface is backed by a real hardware device, by checking
// for a /sys/class/net/<name>/device symlink - present only for interfaces with an actual
// hardware device behind them, not for virtual ones (bridges, veth, tun/tap, docker0, ...).
func isPhysicalInterface(iface psnet.InterfaceStat) bool {
	_, err := os.Lstat("/sys/class/net/" + iface.Name + "/device")
	return err == nil
}
