// Package connectivity connects local applications to Rust-backed XConn sessions.
package connectivity

import (
	"context"
	"fmt"

	api "github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/messages"
	"github.com/xconnio/xconn/bindings/go/protocol"
)

// App describes procedures served by local application sessions.
type App struct {
	Session         *api.Session
	NewSession      func(authID, authRole string) (*api.Session, error)
	Procedures      []string
	Prefixes        []string
	DatagramChannel func(*api.WebRTCDataChannel)
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

// StartAppLayer creates the local yamux boundary used by DeskConn applications.
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
	listener, err := listenAppLayer(router, socketPath)
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

func NewBridge(ctx context.Context, remote *api.Session, app *App) (*api.Bridge, error) {
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

// Datagram channels are a product capability distinct from reliable RPC streams.
// Reject unhandled or old preclassified channels rather than retaining a relay.
func configureDatagrams(provider *api.WebRTCProvider, app *App) {
	provider.OnDataChannel(func(_ string, channel *api.WebRTCDataChannel, first []byte) {
		if first != nil || app.DatagramChannel == nil {
			_ = channel.Close()
			return
		}
		app.DatagramChannel(channel)
	})
}
