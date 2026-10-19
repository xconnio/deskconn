package common

import (
	"fmt"
	"os/exec"
	"strings"
)

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
