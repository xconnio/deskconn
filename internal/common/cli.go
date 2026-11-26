package common

import (
	"context"
	"os/user"
	"path/filepath"

	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
)

// These are helpers the CLI's commands share, desk top among them.

// DaemonURI is the local Unix socket URI. Use ConnectDaemon on all platforms.
func DaemonURI(cfgDirectory string) string {
	return UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// ConnectDaemon joins the current user's local daemon on each supported platform.
func ConnectDaemon(ctx context.Context, cfgDirectory string) (*xconn.Session, error) {
	return connectivity.ConnectApp(ctx, filepath.Join(cfgDirectory, "deskconn.sock"), LocalRealm)
}

// CurrentUsername is the username a standalone machine logs in as unless --username says otherwise.
func CurrentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "deskconn"
}
