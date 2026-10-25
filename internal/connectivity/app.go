// Package xconn connects local applications to Rust-backed XConn sessions.
package connectivity

import (
	"context"
	"fmt"

	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/messages"
	"github.com/xconnio/xconn-core/bindings/go/protocol"
)

// App describes procedures served by local application sessions.
type App struct {
	Session      *api.Session
	NewSession   func(authID, authRole string) (*api.Session, error)
	Procedures   []string
	Prefixes     []string
	StreamSocket string
}

type appAuthorizer struct{}

func (appAuthorizer) Authorize(_ api.BaseSession, message messages.Message) (bool, error) {
	switch message.Type() {
	case messages.MessageTypeCall, messages.MessageTypeRegister, messages.MessageTypeSubscribe:
		return true, nil
	default:
		return false, nil
	}
}

// StartAppLayer creates the Unix RawSocket boundary used by DeskConn applications.
func StartAppLayer(realm, socketPath string) (*api.Router, *api.Listener, *api.Session, error) {
	router, err := api.NewRouter(nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = router.AddRealm(realm, &api.RealmConfig{
		AutoDiscloseCaller: true, Meta: true, Authorizer: appAuthorizer{},
	}); err != nil {
		router.Close()
		return nil, nil, nil, err
	}
	listener, err := api.NewServer(router, nil, nil).ListenAndServeRawSocket(api.NetworkUnix, socketPath)
	if err != nil {
		router.Close()
		return nil, nil, nil, err
	}
	session, err := api.ConnectInMemory(router, realm)
	if err != nil {
		_ = listener.Close()
		router.Close()
		return nil, nil, nil, err
	}
	return router, listener, session, nil
}

// Bridge delegates reusable forwarding behavior to the shared Rust engine.
type Bridge = api.Bridge

func NewBridge(ctx context.Context, remote *api.Session, app *App) (*Bridge, error) {
	if remote == nil || app == nil || app.Session == nil {
		return nil, fmt.Errorf("remote and application sessions are required")
	}
	config := api.BridgeConfig{Target: app.Session}
	for _, procedure := range app.Procedures {
		config.Routes = append(
			config.Routes,
			api.BridgeRoute{Procedure: procedure, Options: api.Options{"invoke": "last"}},
		)
	}
	for _, prefix := range app.Prefixes {
		config.Routes = append(
			config.Routes,
			api.BridgeRoute{Procedure: prefix, Options: api.Options{"match": protocol.MatchPrefix, "invoke": "last"}},
		)
	}
	if app.NewSession != nil {
		config.NewSession = func(_ context.Context, caller api.CallerIdentity) (*api.Session, error) {
			return app.NewSession(caller.AuthID, caller.AuthRole)
		}
	}
	return remote.Bridge(ctx, config)
}

func RegisterBridge(remote *api.Session, app *App) error {
	_, err := NewBridge(context.Background(), remote, app)
	return err
}
