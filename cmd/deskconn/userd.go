package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-go"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// streamCloseGrace is how long shutdown gives xconn to pass the closes of the streams
// still open on to their clients before its connections go down.
const streamCloseGrace = 500 * time.Millisecond

// userdService is the service the user daemon runs as: a systemd user service on Linux,
// a scheduled task on Windows.
const userdService = "deskconn-userd"

// runUserd is the user daemon: the machine's APIs and its connectivity (xconn).
func runUserd(flags userdFlags, standalone bool) {
	detachConsole()

	standaloneKeys, standalonePasswords, err := standaloneAuth(flags, standalone)
	if err != nil {
		serveFatalf("%v", err)
	}
	if standalone && len(standaloneKeys) == 0 && len(standalonePasswords) == 0 {
		serveFatalf("%s", noStandaloneUsersHelp(flags.configPath))
	}
	if (*flags.standaloneCert == "") != (*flags.standaloneKey == "") {
		serveFatalf("--tls-cert and --tls-key go together")
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	var standaloneConfig *xconnd.StandaloneConfig
	if standalone {
		standaloneConfig, err = newStandaloneConfig(cfgDirectory, *flags.standaloneTransport,
			*flags.standaloneQUICAddr, *flags.standaloneWTAddr, *flags.standaloneCert, *flags.standaloneKey)
		if err != nil {
			log.Fatalf("standalone: %v", err)
		}
		standaloneConfig.Keys = standaloneKeys
		standaloneConfig.Passwords = standalonePasswords
	}

	// xconn runs in this process: its app layer serves deskconn.sock (the CLI's local
	// realm), and userd registers every app-layer and CLI-facing procedure on it
	// through an in-memory session.
	appRouter, appListener, appSession, err := xconnd.StartAppLayer(common.LocalRealm,
		filepath.Join(cfgDirectory, "deskconn.sock"))
	if err != nil {
		log.Fatal(err)
	}
	defer appRouter.Close()
	defer appListener.Close()

	clientSessions := userd.NewClientSessions()

	// DISPLAY/WAYLAND_DISPLAY only distinguish a desktop from a headless server on Linux.
	// Windows has no supported headless-server install path, so treat it as desktop
	// unconditionally rather than misreading an unset X11/Wayland var as "server".
	isDesktop := runtime.GOOS != "linux" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""

	var sessionBus *dbus.Conn
	if isDesktop {
		sessionBus, err = dbus.ConnectSessionBus()
		if err != nil {
			log.Printf("session bus unavailable, screen lock/mpris features disabled: %v", err)
			sessionBus = nil
		} else {
			defer sessionBus.Close()
		}
	} else {
		log.Println("no display detected (DISPLAY/WAYLAND_DISPLAY unset), " +
			"running in server mode: display APIs disabled")
	}

	deskconnApis := userd.NewDeskconn(sessionBus, isDesktop, cfgDirectory)
	defer deskconnApis.CloseVPNTunnel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Raw stream relay listener: xconn dials in once per classified
	// remote stream/channel (see xconnd.RelayHeader).
	streamSockPath := filepath.Join(cfgDirectory, "xconn-streams.sock")
	_ = os.Remove(streamSockPath)
	streamListener, err := net.Listen("unix", streamSockPath)
	if err != nil {
		log.Fatal(err)
	}
	defer streamListener.Close()
	common.SafeGo(func() { deskconnApis.ServeStreamRelay(streamListener) })

	// Stream proxy listener: the CLI's raw streams on the persistent machine connections.
	proxySockPath := filepath.Join(cfgDirectory, common.StreamProxySocket)
	_ = os.Remove(proxySockPath)
	proxyListener, err := net.Listen("unix", proxySockPath)
	if err != nil {
		log.Fatal(err)
	}
	defer proxyListener.Close()
	// Owner only: connecting gives access to the user's machines.
	if err := os.Chmod(proxySockPath, 0600); err != nil {
		log.Fatal(err)
	}
	common.SafeGo(func() { userd.ServeStreamProxy(proxyListener, clientSessions, cfgDirectory) })

	session, err := xconn.ConnectInMemory(appRouter, common.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}
	if err := deskconnApis.Register(session); err != nil {
		log.Fatalf("failed to register app-layer procedures: %v", err)
	}
	if err := registerLocalProcedures(session, deskconnApis, clientSessions, cfgDirectory); err != nil {
		log.Fatalf("failed to register CLI-facing procedures: %v", err)
	}

	// xconn exposes the app layer's machine procedures to remote clients, whichever process
	// registered them, and relays their raw streams and WebRTC data channels to
	// streamSockPath.
	xconnApp := &xconnd.App{
		Session: appSession,
		// A session per remote caller, so userd and apps keep each caller's keys.
		NewSession: func() (*xconn.Session, error) {
			return xconn.ConnectInMemory(appRouter, common.LocalRealm)
		},
		Prefixes:     []string{common.ProcedurePrefix, common.ProcedureAdminPrefix},
		StreamSocket: streamSockPath,
	}
	xconnDone := make(chan struct{})
	common.SafeGo(func() {
		defer close(xconnDone)
		if !standalone {
			cloudConfig, err := newCloudConfig(cfgDirectory)
			if err != nil {
				log.Fatal(err)
			}
			if err := xconnd.Run(ctx, xconnApp, cloudConfig); err != nil {
				log.Fatal(err)
			}
			return
		}

		// Standalone: serve the machine realm over QUIC and/or WebTransport to --public-key
		// holders and --user accounts, with no cloud account.
		if err := xconnd.RunStandalone(ctx, xconnApp, standaloneConfig); err != nil {
			log.Fatalf("standalone: %v", err)
		}
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	<-sigChan

	// Close every open stream first, while xconn is still up to pass the closes on to
	// clients, then give it a moment to: a client never learns of a stream that just
	// vanishes with the process (on P2P, it waits forever).
	if deskconnApis.CloseStreams() > 0 {
		time.Sleep(streamCloseGrace)
	}
	cancel()
	<-xconnDone
}

// newStandaloneConfig serves the standalone machine realm over transport (see the transport*
// constants) with the certificate in certFile/keyFile, or else a self-signed one kept in
// cfgDirectory, whose fingerprint clients pin with desk profile add --cert-hash.
func newStandaloneConfig(cfgDirectory, transport, quicAddr, wtAddr, certFile,
	keyFile string) (*xconnd.StandaloneConfig, error) {
	var cert tls.Certificate
	var err error
	if certFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	} else {
		cert, err = common.LoadOrCreateCertificate(filepath.Join(cfgDirectory, "standalone-cert.pem"),
			filepath.Join(cfgDirectory, "standalone-key.pem"))
	}
	if err != nil {
		return nil, err
	}
	fmt.Printf("standalone: certificate SHA-256 fingerprint %s (clients that don't trust its issuer "+
		"pin it with desk profile add --cert-hash %s)\n", common.CertificateFingerprint(cert),
		common.CertificateFingerprint(cert))

	config := &xconnd.StandaloneConfig{
		TLSConfig:  &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		Realm:      common.StandaloneRealm,
		ICEServers: common.ICEServers(),
	}
	if transport == transportQUIC || transport == transportBoth {
		config.QUICAddress = quicAddr
	}
	if transport == transportWebTransport || transport == transportBoth {
		config.WebTransportAddress = wtAddr
	}
	return config, nil
}

// newCloudConfig is how xconn attaches this machine to the deskconn cloud.
func newCloudConfig(cfgDirectory string) (*xconnd.CloudConfig, error) {
	credentialsFile, err := common.CredentialsFilePath()
	if err != nil {
		return nil, err
	}
	return &xconnd.CloudConfig{
		Address:               common.CloudQUICAddress(),
		TLSConfig:             common.CloudQUICTLSConfig(),
		Realm:                 common.CloudRealm,
		ProcedureListKeys:     common.ProcedureDesktopKeyList,
		TopicKeyAddedFormat:   common.TopicDesktopKeyAddedFormat,
		TopicKeyRemovedFormat: common.TopicDesktopKeyRemovedFormat,
		TopicDetachFormat:     common.TopicDeskconnDesktopDetachFormat,
		CredentialsFile:       credentialsFile,
		PrincipalsFile:        filepath.Join(cfgDirectory, "principals.json"),
		ICEServers:            common.ICEServers(),
	}, nil
}

// standaloneAuth gathers who may connect in standalone mode: the --config users and the --user
// ones, whose keys and passwords it returns, plus the --public-key keys. With logLogins set,
// it logs how each can log in.
func standaloneAuth(flags userdFlags, logLogins bool) (keys []string, passwords map[string]string,
	err error) {
	users := slices.Clone(flags.configUsers)
	// --user values of one username add up, e.g. a key per device.
	flagUsers := map[string]int{}
	for _, value := range *flags.standaloneUsers {
		user, err := parseUserFlag(value)
		if err != nil {
			return nil, nil, err
		}
		i, seen := flagUsers[user.Username]
		if !seen {
			flagUsers[user.Username] = len(users)
			users = append(users, user)
			continue
		}
		if user.Password != "" && users[i].Password != "" {
			return nil, nil, fmt.Errorf("--user %q: given two passwords", user.Username)
		}
		users[i].Password += user.Password
		users[i].AuthorizedKeys = append(users[i].AuthorizedKeys, user.AuthorizedKeys...)
	}
	if err := checkUsers(users); err != nil {
		return nil, nil, err
	}
	for _, key := range *flags.standaloneKeys {
		if !isPublicKey(key) {
			return nil, nil, fmt.Errorf("--public-key %q isn't a public key (64 hex digits, see desk keygen)", key)
		}
	}

	passwords = make(map[string]string, len(users))
	for _, user := range users {
		keys = append(keys, user.AuthorizedKeys...)
		if user.Password != "" {
			passwords[user.Username] = user.Password
		}
		if logLogins {
			log.Printf("standalone: %s logs in with %s", user.Username, loginMethods(user))
		}
	}
	keys = append(keys, *flags.standaloneKeys...)
	if logLogins && len(*flags.standaloneKeys) > 0 {
		log.Printf("standalone: %d --public-key key(s) log in as any username", len(*flags.standaloneKeys))
	}
	return keys, passwords, nil
}

// noStandaloneUsersHelp says how to let someone in when standalone mode has no one to: in
// the config file at configPath, if one is used, or else with --user.
func noStandaloneUsersHelp(configPath string) string {
	if configPath != "" {
		return fmt.Sprintf("%s has no users, so no one could connect in standalone mode. Add some under users:\n"+
			"  users:\n"+
			"    - username: alice           # logs in with a password\n"+
			"      password: s3cret\n"+
			"    - username: bob             # logs in with a key (make one with desk keygen)\n"+
			"      authorized-keys:\n"+
			"        - <public-key>\n"+
			"(The sample from desk serve init has these commented out.)", configPath)
	}
	return "--mode standalone needs someone allowed to connect, for example:\n" +
		"  desk serve --mode standalone --user alice:s3cret       alice logs in with a password\n" +
		"  desk serve --mode standalone --user bob:<public-key>   bob logs in with a key (make one with desk keygen)\n" +
		"  desk serve --config serve.yaml                         users from a config file (see desk serve init)\n" +
		"Repeat --user for more users."
}

// parseUserFlag parses a --user value, username:secret. The secret is a public key if it is
// one (64 hex digits, as desk keygen prints it), and a password otherwise.
func parseUserFlag(value string) (serveConfigUser, error) {
	username, secret, ok := strings.Cut(value, ":")
	if !ok || username == "" || secret == "" {
		// Not the value itself: it may hold a password.
		return serveConfigUser{}, errors.New("invalid --user: want username:password or username:public-key")
	}
	if isPublicKey(secret) {
		return serveConfigUser{Username: username, AuthorizedKeys: []string{secret}}, nil
	}
	return serveConfigUser{Username: username, Password: secret}, nil
}

// loginMethods describes how user can log in, e.g. "a password and 2 keys".
func loginMethods(user serveConfigUser) string {
	var methods []string
	if user.Password != "" {
		methods = append(methods, "a password")
	}
	switch n := len(user.AuthorizedKeys); n {
	case 0:
	case 1:
		methods = append(methods, "a key")
	default:
		methods = append(methods, fmt.Sprintf("%d keys", n))
	}
	return strings.Join(methods, " and ")
}

// registerLocalProcedures registers the CLI-facing procedures on session.
func registerLocalProcedures(session *xconn.Session, deskconnApis *userd.Deskconn,
	clientSessions *userd.ClientSessions, cfgDirectory string) error {
	handlers := map[string]xconn.InvocationHandler{
		common.ProcedureProxyFileOp:       userd.ProxyFileOpHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyMachineInfo:  userd.ProxyMachineInfoHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyPing:         userd.ProxyPingHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyCat:          userd.ProxyCatHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyVPNStart:     userd.ProxyVPNStartHandler(deskconnApis),
		common.ProcedureProxyVPNStop:      userd.ProxyVPNStopHandler(deskconnApis),
		common.ProcedureProxyPrinterList:  userd.ProxyPrinterListHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyPrinterPrint: userd.ProxyPrinterPrintHandler(clientSessions, cfgDirectory),
		common.ProcedureLogin: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSessions.Login()
			return xconn.NewInvocationResult()
		},
		common.ProcedureLogout: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSessions.Logout()
			return xconn.NewInvocationResult()
		},
		common.ProcedureConnect: func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
			}
			_, err = clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
			}
			return xconn.NewInvocationResult()
		},
		common.ProcedureDisconnect: func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
			}
			clientSessions.Disconnect(realm)
			return xconn.NewInvocationResult()
		},
		common.ProcedureDisconnectAll: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSessions.DisconnectAll()
			return xconn.NewInvocationResult()
		},
		common.ProcedureConnectedMachines: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult(clientSessions.MachineSessions())
		},
	}

	for uri, handler := range handlers {
		if resp := session.Register(uri, handler).Do(); resp.Err != nil {
			return resp.Err
		}
	}
	return nil
}
