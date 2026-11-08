// Host identity adapter from xconn-go (MIT).
package connectivity

import (
	"os"
	"strings"
)

// MachineID returns a unique, stable identifier for the current host.
func MachineID() (string, error) {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
