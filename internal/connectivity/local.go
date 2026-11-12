package connectivity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	api "github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

// Windows uses authenticated loopback yamux because the native SDK cannot bind
// Unix sockets there. The endpoint file is readable only by its owner.
type localEndpoint struct {
	Address string `json:"address"`
	Secret  string `json:"secret"`
}

func listenLoopbackApp(router *api.Router, path string) (*api.Listener, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	endpoint := localEndpoint{Secret: hex.EncodeToString(secret)}
	policy := localAuthenticator{secret: endpoint.Secret}
	listener, err := api.NewServer(router, policy, nil).ListenAndServeYamux(api.NetworkTCP, "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	endpoint.Address = listener.Address
	if err := writeLocalEndpoint(path, endpoint); err != nil {
		_ = listener.Close()
		return nil, err
	}
	owned := &localAppListener{Listener: listener, path: path, secret: endpoint.Secret}
	return api.NewListener(owned, listener.Addr()), nil
}

type localAuthenticator struct {
	secret string
}

func (localAuthenticator) Methods() []auth.Method {
	return []auth.Method{auth.WAMPCRA}
}

func (a localAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if request.AuthMethod() != auth.WAMPCRA || request.AuthID() != "local" {
		return nil, fmt.Errorf("local endpoint credentials required")
	}
	return auth.NewCRAResponse("local", "anonymous", a.secret, 0), nil
}

func writeLocalEndpoint(path string, endpoint localEndpoint) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".deskconn-endpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = secureLocalEndpoint(file); err == nil {
		//nolint:gosec // The endpoint secret is stored only in this owner-protected file.
		err = json.NewEncoder(file).Encode(endpoint)
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func readLocalEndpoint(path string) (localEndpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return localEndpoint{}, err
	}
	var endpoint localEndpoint
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return endpoint, err
	}
	return endpoint, nil
}

func connectLoopbackApp(ctx context.Context, path, realm string) (*api.Session, error) {
	endpoint, err := readLocalEndpoint(path)
	if err != nil {
		return nil, err
	}
	if endpoint.Address == "" || endpoint.Secret == "" {
		return nil, fmt.Errorf("invalid local daemon endpoint")
	}
	client := api.Client{RoutedOnly: true, Authenticator: auth.NewWAMPCRAAuthenticator("local", endpoint.Secret, nil)}
	return client.Connect(ctx, "yamux://"+endpoint.Address, realm)
}

type localAppListener struct {
	*api.Listener
	path   string
	secret string
}

func (l *localAppListener) Close() error {
	err := l.Listener.Close()
	endpoint, readErr := readLocalEndpoint(l.path)
	if readErr == nil && endpoint.Secret == l.secret {
		err = errors.Join(err, os.Remove(l.path))
	}
	return err
}
