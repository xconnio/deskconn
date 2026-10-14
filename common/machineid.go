package common

import (
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// MachineID returns a unique, stable identifier for the current host.
func MachineID() (string, error) {
	return xconnd.MachineID()
}
