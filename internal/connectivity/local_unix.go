//go:build !windows

package connectivity

import (
	"context"
	"os"

	api "github.com/xconnio/xconn/bindings/go"
)

func listenAppLayer(router *api.Router, path string) (*api.Listener, error) {
	return api.NewServer(router, nil, nil).ListenAndServeYamux(api.NetworkUnix, path)
}

// ConnectApp joins an application's local yamux connection.
func ConnectApp(ctx context.Context, path, realm string) (*api.Session, error) {
	return api.ConnectAnonymous(ctx, "unix+yamux://"+path, realm)
}

func secureLocalEndpoint(file *os.File) error {
	return file.Chmod(0600)
}
