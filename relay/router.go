// Package relay is the Deskconn cloud router: the cloud realm, a realm per machine, and the
// relay that carries clients' raw streams to machines. Who may join, and which machines
// exist, are left to callbacks (see Config), so it runs with any account backend.
package relay

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const (
	// CloudRealm is the realm users, services and apps join; each machine also gets a realm
	// of its own (see Machine).
	CloudRealm = "io.xconn.deskconn"

	// ProcedureAddRealm adds a machine at runtime: called with its realm and authid.
	ProcedureAddRealm = "io.xconn.deskconn.realm.add"
	// ProcedureRemoveRealm removes a machine at runtime: called with its realm and authid.
	ProcedureRemoveRealm = "io.xconn.deskconn.realm.remove"

	// DeskconndPrefix is where a machine's procedures live, in its realm.
	DeskconndPrefix = "io.xconn.deskconn."
	// AdminPrefix is where a machine's procedures for only some of its users live: which,
	// is up to Config.UserRoles.
	AdminPrefix = "io.xconn.deskconn.admin."

	procedureWebRTCOffer     = "io.xconn.webrtc.offer"
	topicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"
	topicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"

	machineAuthRolePrefix = "xconnio:deskconn:desktop:"

	errInvalidArgument = "wamp.error.invalid_argument"
	errOperationFailed = "wamp.error.operation_failed"

	// MatchExact, MatchPrefix and MatchWildcard are the match policies of a permission's URI.
	MatchExact    = "exact"
	MatchPrefix   = "prefix"
	MatchWildcard = "wildcard"
)

// Machine is a machine the router serves, in a realm of its own. Its daemon joins that realm
// as AuthID with MachineAuthRole(AuthID).
type Machine struct {
	AuthID string
	Realm  string
}

// MachineAuthRole is the role Config.Authenticate gives machine authid's daemon.
func MachineAuthRole(authid string) string {
	return machineAuthRolePrefix + authid
}

// Config is what the router leaves to the deployment. Authenticate is required; the rest
// is optional.
type Config struct {
	// Authenticate admits a session joining any realm, with WAMP-CRA or CryptoSign, by
	// returning its authid and role, or refuses it with an error. Machines' daemons must get
	// MachineAuthRole, and users of a machine one of UserRoles.
	Authenticate func(request auth.Request) (auth.Response, error)

	// Machines returns the machines to serve from startup. Others come and go at runtime
	// through Router.AddMachine and Router.RemoveMachine, or ProcedureAddRealm and
	// ProcedureRemoveRealm for whichever roles Roles lets call them.
	Machines func(ctx context.Context) ([]Machine, error)

	// Roles are the cloud realm's roles, besides the ones the router adds per machine.
	Roles []xconn.RealmRole

	// MachinePermissions returns what machine authid's daemon may do on the cloud realm.
	MachinePermissions func(authid string) []xconn.Permission

	// UserRoles are the roles users get in a machine's realm, with what each may do there
	// (see MachineUserPermissions). Users with any of them may also relay raw streams to the
	// machine. Which machines a user may join, and with which role, is up to Authenticate.
	UserRoles []xconn.RealmRole

	// Where to listen. An empty address leaves that transport off.
	RawSocketAddress    string
	QUICAddress         string
	WebTransportAddress string
	// CertHashAddress serves the WebTransport certificate's fingerprint over HTTP, for
	// browsers to trust a self-signed one. Only used without TLSConfig.
	CertHashAddress string

	// TLSConfig is the QUIC and WebTransport certificate. Without it, self-signed ones are
	// generated.
	TLSConfig *tls.Config
}

// Router serves the cloud realm and the machines' realms.
type Router struct {
	config  Config
	router  *xconn.Router
	session *xconn.Session
	relay   *connBroker
}

// New sets up the realms, including those of config.Machines, ready to Serve.
func New(ctx context.Context, config Config) (*Router, error) {
	if config.Authenticate == nil {
		return nil, errors.New("router: Config.Authenticate is required")
	}

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		return nil, err
	}
	if err := router.AddRealm(CloudRealm, &xconn.RealmConfig{AutoDiscloseCaller: true, Roles: config.Roles}); err != nil {
		return nil, err
	}
	if err := router.EnableMetaAPI(CloudRealm); err != nil {
		return nil, err
	}

	userRoles := make(map[string]bool, len(config.UserRoles))
	for _, role := range config.UserRoles {
		userRoles[role.Name] = true
	}
	r := &Router{config: config, router: router, relay: newConnBroker(userRoles)}

	if config.Machines != nil {
		machines, err := config.Machines(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get machines: %w", err)
		}
		for _, machine := range machines {
			if err := r.AddMachine(machine); err != nil {
				return nil, fmt.Errorf("failed to add machine %s: %w", machine.AuthID, err)
			}
		}
	}

	r.session, err = xconn.ConnectInMemory(router, CloudRealm)
	if err != nil {
		return nil, err
	}
	if err := r.registerProcedures(); err != nil {
		return nil, err
	}
	return r, nil
}

// Session is the router's own session on the cloud realm, e.g. for Config.Authenticate to
// ask a service there.
func (r *Router) Session() *xconn.Session {
	return r.session
}

// Serve listens on config's addresses until ctx is done.
func (r *Router) Serve(ctx context.Context) error {
	server := xconn.NewServer(r.router, authenticator{authenticate: r.config.Authenticate}, &xconn.ServerConfig{
		KeepAliveInterval: 30 * time.Second,
		KeepAliveTimeout:  10 * time.Second,
	})

	if r.config.RawSocketAddress != "" {
		listener, err := server.ListenAndServeRawSocket(xconn.NetworkTCP, r.config.RawSocketAddress)
		if err != nil {
			return fmt.Errorf("failed to start raw socket listener: %w", err)
		}
		defer listener.Close()
		log.Printf("raw socket listening on %s", listener.Addr())
	}

	if r.config.QUICAddress != "" {
		tlsConfig := r.config.TLSConfig
		if tlsConfig == nil {
			var err error
			if tlsConfig, err = xconn.GenerateSelfSignedTLSConfig(); err != nil {
				return err
			}
		}
		listener, err := server.ListenAndServeQUIC(r.config.QUICAddress, tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to start QUIC listener: %w", err)
		}
		defer listener.Close()
		log.Printf("QUIC listening on %s", r.config.QUICAddress)

		go func() {
			for conn := range listener.AcceptSession() {
				go r.relay.onDeviceConnect(conn.Ctx, conn.Session, conn.QUICConn)
			}
		}()
		go func() {
			for stream := range listener.AcceptStream() {
				go r.relay.onClientStream(stream.Conn, stream.Session)
			}
		}()
	}

	if r.config.WebTransportAddress != "" {
		tlsConfig := r.config.TLSConfig
		if tlsConfig == nil {
			var certHash []byte
			var err error
			if tlsConfig, certHash, err = xconn.GenerateWebTransportTLSConfig(); err != nil {
				return err
			}
			if r.config.CertHashAddress != "" {
				// Browsers trust a self-signed certificate through serverCertificateHashes,
				// without installing a CA.
				go serveCertHash(r.config.CertHashAddress, base64.StdEncoding.EncodeToString(certHash))
			}
		}
		listener, err := server.ListenAndServeWebTransport(r.config.WebTransportAddress, tlsConfig, "/wamp")
		if err != nil {
			return fmt.Errorf("failed to start WebTransport listener: %w", err)
		}
		defer listener.Close()
		log.Printf("WebTransport listening on %s", r.config.WebTransportAddress)

		go func() {
			for range listener.AcceptSession() {
				// Browser WAMP sessions are fully handled by the router.
			}
		}()
		go func() {
			for stream := range listener.AcceptStream() {
				go r.relay.onClientStream(stream.Conn, stream.Session)
			}
		}()
	}

	<-ctx.Done()
	return nil
}

// MachineUserPermissions is what a user needs in a machine's realm to use it: call its
// procedures under DeskconndPrefix and signal WebRTC for direct connections.
func MachineUserPermissions() []xconn.Permission {
	return []xconn.Permission{
		{URI: DeskconndPrefix, MatchPolicy: MatchPrefix, AllowCall: true},
		{URI: procedureWebRTCOffer, MatchPolicy: MatchExact, AllowCall: true},
		{URI: topicAnswererOnCandidate, MatchPolicy: MatchExact, AllowPublish: true},
		{URI: topicOffererOnCandidate, MatchPolicy: MatchExact, AllowSubscribe: true},
	}
}

// AddMachine serves machine's realm and adds its role to the cloud realm.
func (r *Router) AddMachine(machine Machine) error {
	role := MachineAuthRole(machine.AuthID)
	roles := append([]xconn.RealmRole{
		{
			Name: role,
			Permissions: []xconn.Permission{
				{URI: DeskconndPrefix, MatchPolicy: MatchPrefix, AllowRegister: true},
				{URI: AdminPrefix, MatchPolicy: MatchPrefix, AllowRegister: true},
				{URI: procedureWebRTCOffer, MatchPolicy: MatchExact, AllowRegister: true},
				{URI: topicAnswererOnCandidate, MatchPolicy: MatchExact, AllowSubscribe: true},
				{URI: topicOffererOnCandidate, MatchPolicy: MatchExact, AllowPublish: true},
				{URI: "wamp.session.on_leave", MatchPolicy: MatchExact, AllowSubscribe: true},
				{URI: "io.xconn.deskconn.desktop.detach", MatchPolicy: MatchExact, AllowCall: true},
			},
		},
	}, r.config.UserRoles...)
	err := r.router.AddRealm(machine.Realm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		// The machine follows its callers leaving (wamp.session.on_leave) to drop their keys.
		Meta:  true,
		Roles: roles,
	})
	if err != nil {
		return err
	}

	var permissions []xconn.Permission
	if r.config.MachinePermissions != nil {
		permissions = r.config.MachinePermissions(machine.AuthID)
	}
	return r.router.AddRealmRole(CloudRealm, xconn.RealmRole{Name: role, Permissions: permissions})
}

// RemoveMachine stops serving machine's realm and drops its role from the cloud realm.
func (r *Router) RemoveMachine(machine Machine) error {
	r.router.RemoveRealm(machine.Realm)
	return r.router.RemoveRealmRole(CloudRealm, MachineAuthRole(machine.AuthID))
}

// registerProcedures registers ProcedureAddRealm and ProcedureRemoveRealm.
func (r *Router) registerProcedures() error {
	machineHandler := func(apply func(Machine) error) xconn.InvocationHandler {
		return func(_ context.Context, invocation *xconn.Invocation) *xconn.InvocationResult {
			if len(invocation.Args()) != 2 {
				return xconn.NewInvocationError(errInvalidArgument, "must be called with two arguments (realm and authid)")
			}
			realm, err := invocation.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(errInvalidArgument, err.Error())
			}
			authid, err := invocation.ArgString(1)
			if err != nil {
				return xconn.NewInvocationError(errInvalidArgument, err.Error())
			}
			if err := apply(Machine{AuthID: authid, Realm: realm}); err != nil {
				return xconn.NewInvocationError(errOperationFailed, err.Error())
			}
			return xconn.NewInvocationResult()
		}
	}

	for procedure, handler := range map[string]xconn.InvocationHandler{
		ProcedureAddRealm:    machineHandler(r.AddMachine),
		ProcedureRemoveRealm: machineHandler(r.RemoveMachine),
	} {
		if resp := r.session.Register(procedure, handler).Do(); resp.Err != nil {
			return resp.Err
		}
		log.Printf("Registered procedure %s", procedure)
	}
	return nil
}

// authenticator hands every authentication to Config.Authenticate.
type authenticator struct {
	authenticate func(request auth.Request) (auth.Response, error)
}

func (authenticator) Methods() []auth.Method {
	return []auth.Method{auth.MethodCRA, auth.MethodCryptoSign}
}

func (a authenticator) Authenticate(request auth.Request) (auth.Response, error) {
	return a.authenticate(request)
}

func isMachineSession(session xconn.BaseSession) bool {
	return strings.HasPrefix(session.AuthRole(), machineAuthRolePrefix)
}

func serveCertHash(address, hashBase64 string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wt-cert-hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		fmt.Fprintf(w, `{"hash":%q}`, hashBase64)
	})
	log.Infof("WebTransport cert hash available at http://%s/wt-cert-hash", address)
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		log.Errorf("cert hash server: %v", err)
	}
}
