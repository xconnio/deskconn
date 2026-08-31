package xlink

import (
	"fmt"

	"github.com/grandcat/zeroconf"

	"github.com/xconnio/deskconn/common"
)

func AdvertiseService(hostname string, port int, realm string) (*zeroconf.Server, error) {
	mid, err := common.MachineID()
	if err != nil {
		return nil, err
	}

	txt := []string{
		"realm=" + realm,
		"machineid=" + mid,
		"path=/ws",
	}

	instanceName := fmt.Sprintf("xlink (%s)", hostname)

	return zeroconf.Register(instanceName, "_xconn._tcp", "local.", port, txt, nil)
}
