package common

import (
	"fmt"
	"os/user"
	"path/filepath"
	"time"
)

// These are helpers the CLI's commands share, desk top among them.

// DaemonURI is the address of deskconn.sock, the local daemon's socket.
func DaemonURI(cfgDirectory string) string {
	return UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// CurrentUsername is the authid a profile presents unless --authid says otherwise.
func CurrentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "deskconn"
}

// ShortID is the start of a machine's ID that desk ls shows.
func ShortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[:6]
}

// ConnectedSince is how long ago the daemon connected to realm's machine, from its
// connected machines, or "" if it isn't connected.
func ConnectedSince(connectedMachines map[string]any, realm string) string {
	v, ok := connectedMachines[realm]
	if !ok {
		return ""
	}
	var ts int64
	switch t := v.(type) {
	case int64:
		ts = t
	case uint64:
		ts = int64(t) // nolint: gosec
	case float64:
		ts = int64(t)
	default:
		return "connected"
	}
	return FormatSince(time.Since(time.Unix(ts, 0)))
}

// FormatSince is d the short way: 45s, 12m, 3h, 3h5m.
func FormatSince(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
