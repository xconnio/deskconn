package deskconn

import (
	"fmt"
	"os"
	"strings"

	"github.com/grandcat/zeroconf"
)

func AdvertiseService(hostname string, port int, realm string) (*zeroconf.Server, error) {
	mid, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return nil, err
	}

	txt := []string{
		"realm=" + realm,
		"machineid=" + strings.TrimSpace(string(mid)),
		"path=/ws",
	}

	instanceName := fmt.Sprintf("deskconnd (%s)", hostname)

	return zeroconf.Register(instanceName, "_xconn._tcp", "local.", port, txt, nil)
}
