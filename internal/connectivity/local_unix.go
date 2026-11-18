//go:build !windows

package connectivity

import (
	"context"
	"os"

	api "github.com/xconnio/xconn/bindings/go"
)

// listenAppLayer serves the local realm on a Unix socket only its owner can connect to,
// whatever umask the daemon runs with.
func listenAppLayer(router *api.Router, path string) (*api.Listener, error) {
	listener, err := api.NewServer(router, nil, nil).ListenAndServeYamux(api.NetworkUnix, path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// ConnectApp joins an application's local yamux connection.
func ConnectApp(ctx context.Context, path, realm string) (*api.Session, error) {
	return api.ConnectAnonymous(ctx, "unix+yamux://"+path, realm)
}

func secureLocalEndpoint(file *os.File) error {
	return file.Chmod(0600)
}
