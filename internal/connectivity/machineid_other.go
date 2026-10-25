//go:build !linux && !darwin

package connectivity

import "fmt"

func MachineID() (string, error) {
	return "", fmt.Errorf("configure CloudConfig.MachineID on this platform")
}
