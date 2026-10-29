//go:build linux

package rootd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// runNetCmd runs a privileged network configuration command (ip/iptables/nft)
// with a fixed argv -- never through a shell -- and folds its output into
// the error so failures are diagnosable from logs alone.
func runNetCmd(name string, args ...string) error {
	return runNetCmdInput("", name, args...)
}

// runNetCmdInput is runNetCmd with input fed to the command's stdin.
func runNetCmdInput(input, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ipFamily returns the "ip" flag and host-route prefix length for ip.
func ipFamily(ip string) (flag, hostBits string, err error) {
	parsed := net.ParseIP(ip)
	switch {
	case parsed == nil:
		return "", "", fmt.Errorf("invalid ip address %q", ip)
	case parsed.To4() != nil:
		return "-4", "/32", nil
	default:
		return "-6", "/128", nil
	}
}

// AddHostRoute pins ip to gateway/iface (the machine's normal route, from
// GetDefaultRoute) so it keeps working after the default route is replaced.
// Used to make sure the WebRTC transport itself never gets routed into the
// tunnel it's carrying. ip may be IPv4 or IPv6. A pre-existing identical
// route is not an error.
func AddHostRoute(ip, gateway, iface string) error {
	flag, bits, err := ipFamily(ip)
	if err != nil {
		return err
	}
	args := []string{flag, "route", "add", ip + bits}
	if gateway != "" {
		args = append(args, "via", gateway)
	}
	args = append(args, "dev", iface)

	if err := runNetCmd("ip", args...); err != nil && !strings.Contains(err.Error(), "File exists") {
		return err
	}
	return nil
}

// DelHostRoute removes a route previously added by AddHostRoute.
func DelHostRoute(ip string) error {
	flag, bits, err := ipFamily(ip)
	if err != nil {
		return err
	}
	return runNetCmd("ip", flag, "route", "del", ip+bits)
}

// defaultHalves splits each family's whole address space in two. Routing both halves over a
// device takes over from the default route the way OpenVPN's def1 and WireGuard do, without
// touching the default route itself: being more specific, they win whatever its metric is,
// so nothing goes wrong with several default routes (e.g. NetworkManager's ethernet and wifi
// ones, at different metrics) or with whichever network manager owns it, and removing them is
// all it takes to undo.
func defaultHalves(ipVersion int) []string {
	if ipVersion == 6 {
		return []string{"::/1", "8000::/1"}
	}
	return []string{"0.0.0.0/1", "128.0.0.0/1"}
}

func familyFlag(ipVersion int) string {
	if ipVersion == 6 {
		return "-6"
	}
	return "-4"
}

// ReplaceDefaultRoute routes all of ipVersion's traffic over iface (a TUN device, so no
// gateway is needed) -- see defaultHalves.
func ReplaceDefaultRoute(ipVersion int, iface string) error {
	flag, halves := familyFlag(ipVersion), defaultHalves(ipVersion)
	for i, dst := range halves {
		if err := runNetCmd("ip", flag, "route", "replace", dst, "dev", iface); err != nil {
			for _, added := range halves[:i] {
				_ = runNetCmd("ip", flag, "route", "del", added, "dev", iface)
			}
			return err
		}
	}
	return nil
}

// RestoreDefaultRoute undoes ReplaceDefaultRoute(ipVersion, iface). Routes already gone with
// iface itself are not an error.
func RestoreDefaultRoute(ipVersion int, iface string) error {
	flag := familyFlag(ipVersion)
	var firstErr error
	for _, dst := range defaultHalves(ipVersion) {
		err := runNetCmd("ip", flag, "route", "del", dst, "dev", iface)
		if err != nil && !isNoSuchRoute(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// BlockIPv6Default makes all IPv6 destinations without a more specific route unreachable, so
// a v4-only tunnel can't be bypassed by v6 traffic -- see defaultHalves for why this leaves
// the default route itself alone.
func BlockIPv6Default() error {
	halves := defaultHalves(6)
	for i, dst := range halves {
		if err := runNetCmd("ip", "-6", "route", "replace", "unreachable", dst); err != nil {
			for _, added := range halves[:i] {
				_ = runNetCmd("ip", "-6", "route", "del", "unreachable", added)
			}
			return err
		}
	}
	return nil
}

// RestoreIPv6Default undoes BlockIPv6Default.
func RestoreIPv6Default() error {
	var firstErr error
	for _, dst := range defaultHalves(6) {
		err := runNetCmd("ip", "-6", "route", "del", "unreachable", dst)
		if err != nil && !isNoSuchRoute(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// isNoSuchRoute reports whether err is "ip route del" failing for a route that isn't there.
func isNoSuchRoute(err error) bool {
	return strings.Contains(err.Error(), "No such process") || strings.Contains(err.Error(), "Cannot find device")
}

// SetSysctl writes value to the /proc/sys node for key (e.g.
// "net.ipv4.ip_forward") and returns the value it had before, for the
// caller to restore on teardown.
func SetSysctl(key, value string) (previous string, err error) {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	previous = strings.TrimSpace(string(data))

	if err := os.WriteFile(path, []byte(value), 0644); err != nil { //nolint:gosec
		return previous, fmt.Errorf("write %s: %w", path, err)
	}
	return previous, nil
}
