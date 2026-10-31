package connectivity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	api "github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
	"github.com/xconnio/xconn-core/bindings/go/protocol"
)

const cloudEventDetach = "detach"

const (
	ProcedureWebRTCOffer     = "io.xconn.webrtc.offer"
	TopicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"
	TopicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"
	DefaultSTUNServer        = "stun:stun.l.google.com:19302"
)

// CloudConfig describes the cloud rendezvous and authorization service.
type CloudConfig struct {
	Address               string
	TLSConfig             *tls.Config
	RootCertificates      [][]byte
	Realm                 string
	ProcedureListKeys     string
	TopicKeyAddedFormat   string
	TopicKeyRemovedFormat string
	TopicDetachFormat     string
	CredentialsFile       string
	PrincipalsFile        string
	ICEServers            []api.WebRTCICEServer
	MachineID             string
	RetryInterval         time.Duration
	OnError               func(error)
}

type cloudEvent struct {
	kind  string
	event *api.Event
}

func DefaultICEServers() []api.WebRTCICEServer {
	return []api.WebRTCICEServer{{URLs: []string{DefaultSTUNServer}}}
}

// Run waits for attachment, reconnects cloud sessions, and maintains direct-client authorization.
// All remote transport, crypto and WAMP processing are provided by Rust.
func Run(ctx context.Context, app *App, config *CloudConfig) error {
	if config == nil || app == nil || app.Session == nil || config.Address == "" || config.Realm == "" {
		return fmt.Errorf("cloud configuration and application are required")
	}

	if config.CredentialsFile == "" || config.PrincipalsFile == "" || config.ProcedureListKeys == "" {
		return fmt.Errorf("credentials path, principals path and key-list procedure are required")
	}
	machineID := config.MachineID
	if machineID == "" {
		var err error
		machineID, err = MachineID()
		if err != nil {
			return err
		}
	}
	for {
		credentials, err := EnsureCredentials(ctx, config.CredentialsFile)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil
		}
		if err != nil {
			return err
		}
		detached, err := runAttached(ctx, app, config, credentials, machineID)
		if err != nil {
			return err
		}
		if !detached {
			return nil
		}
		if err = os.Remove(config.CredentialsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}

func NewDeviceRouter(realm string) (*api.Router, error) {
	router, err := api.NewRouter(nil)
	if err != nil {
		return nil, err
	}
	permissions := []api.Permission{{URI: "io.xconn.", MatchPolicy: protocol.MatchPrefix, AllowCall: true},
		{URI: "io.xconn.webrtc.", MatchPolicy: protocol.MatchPrefix, AllowPublish: true, AllowSubscribe: true}}
	if err = router.AddRealm(realm, &api.RealmConfig{AutoDiscloseCaller: true, Meta: true, Roles: []api.RealmRole{
		{Name: "owner", Permissions: permissions},
		{Name: "admin", Permissions: permissions},
		{Name: "member", Permissions: permissions},
	}}); err != nil {
		router.Close()
		return nil, err
	}
	return router, nil
}

func runAttached(
	ctx context.Context,
	app *App,
	config *CloudConfig,
	credentials *Credentials,
	machineID string,
) (bool, error) {
	principals, err := ReadPrincipalsFromFile(config.PrincipalsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	policy := NewAuthenticator(principals, config.PrincipalsFile)
	router, err := NewDeviceRouter(credentials.Realm)
	if err != nil {
		return false, err
	}
	defer router.Close()
	local, err := api.ConnectInMemory(router, credentials.Realm)
	if err != nil {
		return false, err
	}
	defer local.Close()
	bridge, err := NewBridge(ctx, local, app)
	if err != nil {
		return false, err
	}
	defer bridge.Close()
	detached := false
	err = api.RunAttachment(ctx, api.AttachmentRetryPolicy{Initial: config.RetryInterval},
		func(attempt context.Context) (api.AttachmentOutcome, error) {
			var cycleError error
			detached, cycleError = cloudCycle(attempt, app, config, credentials, machineID, router, policy)
			if detached {
				return api.AttachmentDetached, nil
			}
			var disconnected *cloudDisconnected
			if errors.As(cycleError, &disconnected) {
				return api.AttachmentDisconnected, cycleError
			}
			return api.AttachmentRetry, cycleError
		}, func(err error) {
			if config.OnError != nil {
				config.OnError(err)
			} else {
				log.Printf("deskconn cloud reconnect: %v", err)
			}
		})
	if ctx.Err() != nil {
		return false, nil
	}
	return detached, err
}

func cloudCycle(
	parent context.Context,
	app *App,
	config *CloudConfig,
	credentials *Credentials,
	machineID string,
	router *api.Router,
	policy *Authenticator,
) (detached bool, cycleError error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	detachedEvent := make(chan struct{})
	var detachOnce sync.Once
	defer func() {
		select {
		case <-detachedEvent:
			detached = true
			cycleError = nil
		default:
		}
	}()
	initialization, stopInitialization := context.WithTimeout(ctx, 15*time.Second)
	defer stopInitialization()
	tlsConfig := config.TLSConfig
	if config.RootCertificates != nil {
		if tlsConfig == nil {
			tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		} else {
			tlsConfig = tlsConfig.Clone()
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		for _, der := range config.RootCertificates {
			certificate, err := x509.ParseCertificate(der)
			if err != nil {
				return false, err
			}
			tlsConfig.RootCAs.AddCert(certificate)
		}
	}
	connection, err := api.DialQUICWithTLS(initialization, config.Address, tlsConfig)
	if err != nil {
		return false, err
	}
	defer connection.Close()
	authenticator, err := auth.NewCryptoSignAuthenticator(credentials.AuthID, credentials.PrivateKey, nil)
	if err != nil {
		return false, err
	}
	client := &api.Client{SerializerSpec: api.CBORSerializerSpec, Authenticator: authenticator}
	device, err := connection.OpenSession(initialization, credentials.Realm, client)
	if err != nil {
		if strings.Contains(err.Error(), "wamp.error.no_such_realm") {
			return true, nil
		}
		return false, err
	}
	defer device.Close()
	cloud, err := connection.OpenSession(initialization, config.Realm, client)
	if err != nil {
		return false, err
	}
	defer cloud.Close()
	events := make(chan cloudEvent, 128)
	overflow := make(chan struct{}, 1)
	for _, item := range []struct{ kind, topic string }{
		{"add", config.TopicKeyAddedFormat},
		{"remove", config.TopicKeyRemovedFormat},
		{cloudEventDetach, config.TopicDetachFormat},
	} {
		if item.topic == "" {
			continue
		}
		subscription := cloud.Subscribe(fmt.Sprintf(item.topic, machineID), func(event *api.Event) {
			if item.kind == cloudEventDetach {
				detachOnce.Do(func() { close(detachedEvent) })
				cancel()
				return
			}
			select {
			case events <- cloudEvent{kind: item.kind, event: event}:
			default:
				select {
				case overflow <- struct{}{}:
				default:
				}
				cancel()
			}
		}).ExecuteContext(initialization)
		if subscription.Err != nil {
			return false, subscription.Err
		}
		defer func() { _ = subscription.Unsubscribe() }()
	}
	result := cloud.Call(config.ProcedureListKeys).ExecuteContext(initialization)
	if result.Err != nil {
		return false, result.Err
	}
	values, err := result.ArgList(0)
	if err != nil {
		return false, err
	}
	principals := make([]*CryptosignPrincipal, len(values))
	for index, value := range values {
		var principal CryptosignPrincipal
		if err = value.Decode(&principal); err != nil {
			return false, err
		}
		principals[index] = &principal
	}
	policy.SetPrincipals(principals)
	if err = WritePrincipalsToFile(config.PrincipalsFile, principals); err != nil {
		return false, err
	}
	bridge, err := NewBridge(ctx, device, app)
	if err != nil {
		return false, err
	}
	defer bridge.Close()
	provider := api.NewWebRTCHandler()
	servers := config.ICEServers
	if len(servers) == 0 {
		servers = DefaultICEServers()
	}
	if err = provider.SetupContext(initialization, &api.WebRTCProviderConfig{
		Session: device, Router: router, Authenticator: policy, ICEServers: servers,
		ProcedureHandleOffer: ProcedureWebRTCOffer, TopicHandleRemoteCandidates: TopicAnswererOnCandidate,
		TopicPublishLocalCandidate: TopicOffererOnCandidate,
	}); err != nil {
		return false, err
	}
	relayContext, stopRelays := context.WithCancel(ctx)
	var relayMu sync.Mutex
	var relayWorkers sync.WaitGroup
	relaysClosed := false
	startRelay := func(run func()) bool {
		relayMu.Lock()
		defer relayMu.Unlock()
		if relaysClosed {
			return false
		}
		relayWorkers.Add(1)
		go func() { defer relayWorkers.Done(); run() }()
		return true
	}
	closeRelays := func() error {
		relayMu.Lock()
		relaysClosed = true
		relayMu.Unlock()
		stopRelays()
		_ = provider.Close()
		_ = connection.Close()
		relayWorkers.Wait()
		return nil
	}
	if app.StreamSocket != "" {
		startRelay(func() {
			for {
				channel, err := provider.AcceptChannel(relayContext)
				if err != nil {
					return
				}
				if !startRelay(func() { relayDataChannel(relayContext, channel, app.StreamSocket) }) {
					_ = channel.Close()
					return
				}
			}
		})
		startRelay(func() {
			for {
				stream, err := connection.AcceptStream(relayContext)
				if err != nil {
					return
				}
				if !startRelay(func() { relayStream(relayContext, stream, app.StreamSocket) }) {
					_ = stream.Close()
					return
				}
			}
		})
	}
	attachment, err := api.NewAttachment(ctx, api.AttachmentConfig{
		Sessions: []*api.Session{device, cloud}, Bridges: []*api.Bridge{bridge}, Shutdown: closeRelays,
	})
	if err != nil {
		_ = closeRelays()
		return false, err
	}
	defer attachment.Close()
	attachmentDone := make(chan error, 1)
	go func() { attachmentDone <- attachment.Wait(context.WithoutCancel(ctx)) }()
	stopInitialization()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-parent.Done():
			return false, nil
		case <-overflow:
			return false, fmt.Errorf("cloud authorization event queue overflow")
		case err := <-attachmentDone:
			if err != nil {
				return false, &cloudDisconnected{err.Error()}
			}
			return false, &cloudDisconnected{"upstream attachment disconnected"}
		case update := <-events:
			switch update.kind {
			case cloudEventDetach:
				return true, nil
			case "add":
				err = policy.addKey(update.event)
			case "remove":
				err = policy.removeKeys(update.event)
			}
			if err != nil {
				return false, err
			}
		}
	}
}

type cloudDisconnected struct{ message string }

func (e *cloudDisconnected) Error() string { return e.message }
