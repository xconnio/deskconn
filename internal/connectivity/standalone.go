package connectivity

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"sync"

	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

const WebTransportPath = "/wamp"

// StandaloneConfig serves an application without a cloud attachment.
type StandaloneConfig struct {
	WebSocketAddress    string
	Authenticator       auth.ServerAuthenticator
	QUICAddress         string
	WebTransportAddress string
	TLSConfig           *tls.Config
	Realm               string
	Keys                []string
	Passwords           map[string]string
	ICEServers          []api.WebRTCICEServer
}

type standaloneAuthenticator struct {
	keys      map[string]bool
	passwords map[string]string
}

// NewStandaloneAuthenticator accepts configured public keys and WAMP-CRA users.
// Credentials are copied so callers cannot change the policy during authentication.
func NewStandaloneAuthenticator(
	keys []string,
	passwords map[string]string,
) auth.ServerAuthenticator {
	policy := &standaloneAuthenticator{
		keys:      make(map[string]bool, len(keys)),
		passwords: make(map[string]string, len(passwords)),
	}

	for _, key := range keys {
		policy.keys[key] = true
	}

	for id, password := range passwords {
		policy.passwords[id] = password
	}

	return policy
}

func (a *standaloneAuthenticator) Methods() []auth.Method {
	var methods []auth.Method
	if len(a.keys) != 0 {
		methods = append(methods, auth.MethodCryptoSign)
	}

	if len(a.passwords) != 0 {
		methods = append(methods, auth.WAMPCRA)
	}

	return methods
}

func (a *standaloneAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if crypto, ok := request.(*auth.RequestCryptoSign); ok {
		if !a.keys[crypto.PublicKey()] {
			return nil, fmt.Errorf("unknown public key")
		}
		return auth.NewResponse(request.AuthID(), "owner", 0)
	}

	if request.AuthMethod() != auth.WAMPCRA {
		return nil, fmt.Errorf("unsupported authentication method %s", request.AuthMethod())
	}

	password, exists := a.passwords[request.AuthID()]
	if !exists {
		return nil, fmt.Errorf("unknown authid %s", request.AuthID())
	}

	return auth.NewCRAResponse(request.AuthID(), "owner", password, 0), nil
}

type incomingStreams interface {
	AcceptStream(context.Context) (*api.QUICStream, *api.SessionDetails, error)
}

// RunStandalone owns listeners, application bridges and WebRTC channels until cancellation.
func RunStandalone(parent context.Context, app *App, config *StandaloneConfig) error {
	if app == nil || app.Session == nil || config == nil || config.Realm == "" {
		return fmt.Errorf("standalone configuration and application are required")
	}

	if config.QUICAddress == "" && config.WebTransportAddress == "" {
		return fmt.Errorf("standalone mode needs a QUIC or WebTransport address")
	}

	if config.TLSConfig == nil || len(config.TLSConfig.Certificates) == 0 {
		return fmt.Errorf("standalone mode needs a TLS certificate")
	}

	if config.Authenticator == nil && len(config.Keys) == 0 && len(config.Passwords) == 0 {
		return fmt.Errorf("standalone mode needs an authorized key or user")
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	router, err := NewDeviceRouter(config.Realm)
	if err != nil {
		return err
	}

	defer router.Close()
	policy := config.Authenticator
	if policy == nil {
		policy = NewStandaloneAuthenticator(config.Keys, config.Passwords)
	}
	server := api.NewServer(router, policy, nil)
	defer server.Close()
	if config.WebSocketAddress != "" {
		if _, err := server.ListenAndServeWebSocket(api.NetworkTCP, config.WebSocketAddress); err != nil {
			return err
		}
	}

	var listeners []incomingStreams
	var quicListeners []*api.QUICListener
	certificate := config.TLSConfig.Certificates[0]
	if config.QUICAddress != "" {
		listener, err := server.ListenQUIC(ctx, config.QUICAddress, &certificate)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		quicListeners = append(quicListeners, listener)
	}

	if config.WebTransportAddress != "" {
		listener, err := server.ListenWebTransport(
			ctx,
			config.WebTransportAddress,
			WebTransportPath,
			certificate,
		)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}

	session, err := api.ConnectInMemory(router, config.Realm)
	if err != nil {
		return err
	}

	defer session.Close()
	bridge, err := NewBridge(ctx, session, app)
	if err != nil {
		return err
	}

	defer bridge.Close()
	provider := api.NewWebRTCHandler()
	servers := config.ICEServers
	if len(servers) == 0 {
		servers = DefaultICEServers()
	}

	if err := provider.SetupContext(ctx, &api.WebRTCProviderConfig{
		Session: session, Router: router, Authenticator: policy, ICEServers: servers,
		ProcedureHandleOffer: ProcedureWebRTCOffer, TopicHandleRemoteCandidates: TopicAnswererOnCandidate,
		TopicPublishLocalCandidate: TopicOffererOnCandidate,
	}); err != nil {
		return err
	}

	defer provider.Close()

	// Acceptors are joined before waiting for their relay workers, so no worker
	// can be added concurrently with the final Wait.
	var acceptors, relays sync.WaitGroup
	failures := make(chan error, len(listeners)+len(quicListeners)+1)
	defer func() {
		cancel()
		_ = server.Close()
		_ = provider.Close()
		acceptors.Wait()
		relays.Wait()
	}()
	for _, listener := range quicListeners {
		acceptors.Add(1)
		go func() {
			defer acceptors.Done()
			for {
				connection, _, err := listener.AcceptConnection(ctx)
				if err != nil {
					failures <- err
					return
				}
				relays.Add(1)
				go func() {
					defer relays.Done()
					defer connection.Close()
					_ = connection.Wait(ctx)
				}()
			}
		}()
	}

	for _, listener := range listeners {
		acceptors.Add(1)
		go func() {
			defer acceptors.Done()
			for {
				stream, _, err := listener.AcceptStream(ctx)
				if err != nil {
					failures <- err
					return
				}
				if app.StreamSocket == "" {
					_ = stream.Close()
					continue
				}
				relays.Add(1)
				go func() { defer relays.Done(); relayStream(ctx, stream, app.StreamSocket) }()
			}
		}()
	}

	acceptors.Add(1)
	go func() {
		defer acceptors.Done()
		for {
			channel, err := provider.AcceptChannel(ctx)
			if err != nil {
				failures <- err
				return
			}
			if app.StreamSocket == "" {
				_ = channel.Close()
				continue
			}
			relays.Add(1)
			go func() { defer relays.Done(); relayDataChannel(ctx, channel, app.StreamSocket) }()
		}
	}()
	select {
	case <-parent.Done():
		return nil
	case err := <-failures:
		if parent.Err() != nil {
			return nil
		}
		return err
	}
}

// RelayStream forwards a raw stream through the application's Unix relay socket.
// Closing either endpoint closes both directions and joins the copy workers.
func RelayStream(stream io.ReadWriteCloser, path string) {
	relayStream(context.Background(), stream, path)
}
