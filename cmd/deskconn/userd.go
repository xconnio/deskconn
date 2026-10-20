package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

// streamCloseGrace is how long shutdown gives xlink to pass the closes of the streams
// still open on to their clients before its connections go down.
const streamCloseGrace = 500 * time.Millisecond

// userdService is the service the user daemon runs as: a systemd user service on Linux,
// a scheduled task on Windows.
const userdService = "deskconn-userd"

// runUserd is the user daemon: the machine's APIs and its connectivity (xlink).
func runUserd(flags userdFlags, standalone bool) {
	detachConsole()

	standalonePasswords, err := parseUsers(*flags.standaloneUsers)
	if err != nil {
		serveFatalf("%v", err)
	}
	if standalone && len(*flags.standaloneKeys) == 0 && len(standalonePasswords) == 0 {
		serveFatalf("--mode standalone needs at least one --public-key or --user")
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
		standaloneConfig.Keys = *flags.standaloneKeys
		standaloneConfig.Passwords = standalonePasswords
	}

	// xlink runs in this process: its app layer serves deskconn.sock (the CLI's local
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

	// Raw stream relay listener: xlink dials in once per classified
	// remote stream/channel (see xconnd.RelayHeader).
	streamSockPath := filepath.Join(cfgDirectory, "xlink-streams.sock")
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
	log.Println("registered procedures with xlink")

	// xlink exposes the app layer's machine procedures to remote clients, whichever process
	// registered them, and relays their raw streams and WebRTC data channels to
	// streamSockPath.
	xlinkApp := &xconnd.App{
		Session: appSession,
		// A session per remote caller, so userd and apps keep each caller's keys.
		NewSession: func() (*xconn.Session, error) {
			return xconn.ConnectInMemory(appRouter, common.LocalRealm)
		},
		Prefixes:     []string{common.ProcedurePrefix, common.ProcedureAdminPrefix},
		StreamSocket: streamSockPath,
	}
	xlinkDone := make(chan struct{})
	common.SafeGo(func() {
		defer close(xlinkDone)
		if !standalone {
			cloudConfig, err := newCloudConfig(cfgDirectory)
			if err != nil {
				log.Fatal(err)
			}
			if err := xconnd.Run(ctx, xlinkApp, cloudConfig); err != nil {
				log.Fatal(err)
			}
			return
		}

		// Standalone: serve the machine realm over QUIC and/or WebTransport to --public-key
		// holders and --user accounts, with no cloud account.
		if err := xconnd.RunStandalone(ctx, xlinkApp, standaloneConfig); err != nil {
			log.Fatalf("standalone: %v", err)
		}
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	<-sigChan

	// Close every open stream first, while xlink is still up to pass the closes on to
	// clients, then give it a moment to: a client never learns of a stream that just
	// vanishes with the process (on P2P, it waits forever).
	if deskconnApis.CloseStreams() > 0 {
		time.Sleep(streamCloseGrace)
	}
	cancel()
	<-xlinkDone
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
	log.Printf("standalone: certificate SHA-256 fingerprint %s (clients that don't trust its issuer "+
		"pin it with desk profile add --cert-hash %s)", common.CertificateFingerprint(cert),
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

// newCloudConfig is how xlink attaches this machine to the deskconn cloud.
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

// parseUsers parses --user values (username:password) into a username -> password map.
func parseUsers(users []string) (map[string]string, error) {
	passwords := make(map[string]string, len(users))
	for _, u := range users {
		username, password, ok := strings.Cut(u, ":")
		if !ok || username == "" || password == "" {
			return nil, fmt.Errorf("invalid --user: want username:password")
		}
		if _, dup := passwords[username]; dup {
			return nil, fmt.Errorf("duplicate --user %q", username)
		}
		passwords[username] = password
	}
	return passwords, nil
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
