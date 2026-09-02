package iptun

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runNetCmd runs a privileged network configuration command (ip/iptables)
// with a fixed argv -- never through a shell -- and folds its output into
// the error so failures are diagnosable from logs alone.
func runNetCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DefaultRoute describes an IPv4 or IPv6 default route as reported by
// "ip route show default". Gateway is empty for an on-link (gateway-less)
// default route.
type DefaultRoute struct {
	Iface   string
	Gateway string
}

// GetDefaultRoute returns the current default route for the given IP
// version (4 or 6), taking the first entry if more than one is present.
func GetDefaultRoute(ipVersion int) (*DefaultRoute, error) {
	flag := "-4"
	if ipVersion == 6 {
		flag = "-6"
	}

	out, err := exec.Command("ip", flag, "route", "show", "default").Output()
	if err != nil {
		return nil, fmt.Errorf("ip %s route show default: %w", flag, err)
	}

	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return nil, fmt.Errorf("no ipv%d default route found", ipVersion)
	}

	route := &DefaultRoute{}
	fields := strings.Fields(line)
	for i, f := range fields {
		switch f {
		case "via":
			if i+1 < len(fields) {
				route.Gateway = fields[i+1]
			}
		case "dev":
			if i+1 < len(fields) {
				route.Iface = fields[i+1]
			}
		}
	}
	if route.Iface == "" {
		return nil, fmt.Errorf("could not parse default route: %q", line)
	}

	return route, nil
}

// AddHostRoute pins ip to gateway/iface (the machine's normal route, from
// GetDefaultRoute) so it keeps working after the default route is replaced.
// Used to make sure the WebRTC transport itself never gets routed into the
// tunnel it's carrying. A pre-existing identical route is not an error.
func AddHostRoute(ip, gateway, iface string) error {
	args := []string{"route", "add", ip + "/32"}
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
	return runNetCmd("ip", "route", "del", ip+"/32")
}

// ReplaceDefaultRoute points the default route for ipVersion at iface (a
// TUN device, so no gateway is needed) and returns the route it replaced,
// if any, for RestoreDefaultRoute to put back.
func ReplaceDefaultRoute(ipVersion int, iface string) (*DefaultRoute, error) {
	prev, err := GetDefaultRoute(ipVersion)
	if err != nil {
		prev = nil // nothing to restore later; proceed anyway
	}

	flag := "-4"
	if ipVersion == 6 {
		flag = "-6"
	}
	if err := runNetCmd("ip", flag, "route", "replace", "default", "dev", iface); err != nil {
		return nil, err
	}

	return prev, nil
}

// RestoreDefaultRoute restores the default route captured by
// ReplaceDefaultRoute. A nil prev means there was no default route before,
// so the tunnel's default route is simply removed.
func RestoreDefaultRoute(ipVersion int, prev *DefaultRoute) error {
	flag := "-4"
	if ipVersion == 6 {
		flag = "-6"
	}

	if prev == nil || prev.Iface == "" {
		return runNetCmd("ip", flag, "route", "del", "default")
	}

	args := []string{flag, "route", "replace", "default"}
	if prev.Gateway != "" {
		args = append(args, "via", prev.Gateway)
	}
	args = append(args, "dev", prev.Iface)
	return runNetCmd("ip", args...)
}

// BlockIPv6Default replaces any IPv6 default route with an unreachable
// route, so a v4-only tunnel can't be bypassed by v6 traffic. hadDefault
// reports whether there was an existing default route to restore later.
func BlockIPv6Default() (hadDefault bool, prev *DefaultRoute, err error) {
	prev, gerr := GetDefaultRoute(6)
	hadDefault = gerr == nil

	if err := runNetCmd("ip", "-6", "route", "replace", "unreachable", "default", "metric", "1"); err != nil {
		return hadDefault, prev, err
	}
	return hadDefault, prev, nil
}

// RestoreIPv6Default undoes BlockIPv6Default.
func RestoreIPv6Default(hadDefault bool, prev *DefaultRoute) error {
	if !hadDefault {
		return runNetCmd("ip", "-6", "route", "del", "unreachable", "default", "metric", "1")
	}
	return RestoreDefaultRoute(6, prev)
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

// AddMasquerade adds an iptables MASQUERADE rule so traffic from subnet
// leaving via oif gets the host's own address.
func AddMasquerade(subnet, oif string) error {
	return runNetCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", oif, "-j", "MASQUERADE")
}

// DelMasquerade removes a rule added by AddMasquerade.
func DelMasquerade(subnet, oif string) error {
	return runNetCmd("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet, "-o", oif, "-j", "MASQUERADE")
}

// AddForwardAccept allows forwarding from inIface to outIface. Rules are
// inserted at the head of FORWARD (-I, not -A) so they take effect ahead of
// any earlier DROP/REJECT rule a distro or another tool (e.g. Docker) may
// already have installed further down the chain.
func AddForwardAccept(inIface, outIface string) error {
	return runNetCmd("iptables", "-I", "FORWARD", "-i", inIface, "-o", outIface, "-j", "ACCEPT")
}

// DelForwardAccept removes a rule added by AddForwardAccept.
func DelForwardAccept(inIface, outIface string) error {
	return runNetCmd("iptables", "-D", "FORWARD", "-i", inIface, "-o", outIface, "-j", "ACCEPT")
}

// AddForwardEstablished allows return traffic for connections the tunnel
// initiated.
func AddForwardEstablished(inIface, outIface string) error {
	return runNetCmd("iptables", "-I", "FORWARD", "-i", inIface, "-o", outIface,
		"-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT")
}

// DelForwardEstablished removes a rule added by AddForwardEstablished.
func DelForwardEstablished(inIface, outIface string) error {
	return runNetCmd("iptables", "-D", "FORWARD", "-i", inIface, "-o", outIface,
		"-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT")
}
