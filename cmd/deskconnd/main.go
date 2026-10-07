package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/xconn-go"
	xconnd "github.com/xconnio/xconn-go/xconn"
)

// lanPort is where xlink serves the device realm to clients on the local network.
const lanPort = 18080

// Standalone transports (--transport).
const (
	transportQUIC         = "quic"
	transportWebTransport = "webtransport"
	transportBoth         = "both"
)

func main() {
	app := kingpin.New("deskconnd", "Deskconn daemon: the device's APIs and its connectivity (xlink)")
	standalone := app.Flag("standalone", "Serve this device directly over --transport to --public-key holders "+
		"and --user accounts, instead of through the cloud").Bool()
	standaloneTransport := app.Flag("transport", "Standalone transport to serve: quic, webtransport or both").
		Default(transportQUIC).Enum(transportQUIC, transportWebTransport, transportBoth)
	standaloneQUICAddr := app.Flag("quic-address", "Where to serve QUIC in standalone mode (host:port, UDP)").
		Default("0.0.0.0:18080").String()
	standaloneWTAddr := app.Flag("webtransport-address", "Where to serve WebTransport in standalone mode "+
		"(host:port, UDP)").Default("0.0.0.0:18081").String()
	standaloneCert := app.Flag("tls-cert", "TLS certificate (PEM) for standalone mode; without it, a "+
		"self-signed one is generated and kept in the config directory").String()
	standaloneKey := app.Flag("tls-key", "Private key (PEM) of --tls-cert").String()
	standaloneKeys := app.Flag("public-key", "Public key (hex) allowed to connect in standalone mode; repeat for more "+
		"(see `desk keygen`)").Strings()
	standaloneUsers := app.Flag("user", "username:password allowed to connect in standalone mode; repeat for "+
		"more").Envar("DESKCONND_USERS").Strings()
	kingpin.MustParse(app.Parse(os.Args[1:]))
	standalonePasswords, err := parseUsers(*standaloneUsers)
	if err != nil {
		app.Fatalf("%v", err)
	}
	if *standalone && len(*standaloneKeys) == 0 && len(standalonePasswords) == 0 {
		app.Fatalf("--standalone needs at least one --public-key or --user")
	}
	if (*standaloneCert == "") != (*standaloneKey == "") {
		app.Fatalf("--tls-cert and --tls-key go together")
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	var standaloneConfig *xconnd.StandaloneConfig
	if *standalone {
		standaloneConfig, err = newStandaloneConfig(cfgDirectory, *standaloneTransport, *standaloneQUICAddr,
			*standaloneWTAddr, *standaloneCert, *standaloneKey)
		if err != nil {
			log.Fatalf("standalone: %v", err)
		}
		standaloneConfig.Keys = *standaloneKeys
		standaloneConfig.Passwords = standalonePasswords
	}

	// xlink runs in this process: its app layer serves deskconn.sock (the CLI's local
	// realm), and deskconnd registers every app-layer and CLI-facing procedure on it
	// through an in-memory session.
	appRouter, appListener, appSession, err := xconnd.StartAppLayer(common.LocalRealm,
		filepath.Join(cfgDirectory, "deskconn.sock"))
	if err != nil {
		log.Fatal(err)
	}
	defer appRouter.Close()
	defer appListener.Close()

	clientSessions := deskconnd.NewClientSessions()

	isDesktop := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""

	var screen *deskconnd.Screen
	var mpris *deskconnd.MPRIS
	var audio *deskconnd.Audio

	if isDesktop {
		systemBus, err := dbus.ConnectSystemBus()
		if err != nil {
			log.Fatal(err)
		}
		defer systemBus.Close()

		sessionBus, err := dbus.ConnectSessionBus()
		if err != nil {
			log.Fatal(err)
		}
		defer sessionBus.Close()

		screen = deskconnd.NewScreen(sessionBus, systemBus, cfgDirectory)
		mpris = deskconnd.NewMPRIS(sessionBus)
		audio = deskconnd.NewAudio()
		defer audio.Close()
	} else {
		log.Println("no display detected (DISPLAY/WAYLAND_DISPLAY unset), " +
			"running in server mode: display APIs disabled")
	}

	deskconnApis := deskconnd.NewDeskconn(screen, mpris, audio, isDesktop, cfgDirectory)
	defer deskconnApis.CloseVPNTunnel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deskconnApis.StartIndexer(ctx)

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

	// Stream proxy listener: the CLI's raw streams on the persistent device connections.
	proxySockPath := filepath.Join(cfgDirectory, common.StreamProxySocket)
	_ = os.Remove(proxySockPath)
	proxyListener, err := net.Listen("unix", proxySockPath)
	if err != nil {
		log.Fatal(err)
	}
	defer proxyListener.Close()
	// Owner only: connecting gives access to the user's devices.
	if err := os.Chmod(proxySockPath, 0600); err != nil {
		log.Fatal(err)
	}
	common.SafeGo(func() { deskconnd.ServeStreamProxy(proxyListener, clientSessions, cfgDirectory) })

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

	// xlink exposes the app layer's device procedures to remote clients and relays their
	// raw streams and WebRTC data channels to streamSockPath.
	xlinkApp := &xconnd.App{
		Session: appSession,
		Procedures: []string{
			common.ProcedureKeyExchange,
			common.ProcedureScreenBrightnessGet,
			common.ProcedureScreenBrightnessSet,
			common.ProcedureScreenLock,
			common.ProcedureScreenIsLocked,
			common.ProcedureShellIsBusy,
			common.ProcedureFileBrowse,
			common.ProcedurePrinterList,
			common.ProcedurePrinterPrint,
			common.ProcedureFileRename,
			common.ProcedureFileDelete,
			common.ProcedureFileCopy,
			common.ProcedureFileEdit,
			common.ProcedureFileSearch,
			common.ProcedurePing,
			common.ProcedureIndexQuery,
			common.ProcedureWallpaperGet,
			common.ProcedureWallpaperChecksum,
			common.ProcedureMPRISPlayers,
			common.ProcedureMPRISPlayPause,
			common.ProcedureMPRISPlay,
			common.ProcedureMPRISPause,
			common.ProcedureMPRISNext,
			common.ProcedureMPRISPrevious,
			common.ProcedureAudioMute,
			common.ProcedureAudioUnmute,
			common.ProcedureAudioToggleMute,
			common.ProcedureAudioIsMuted,
			common.ProcedureScreenshot,
			common.ProcedureScreenshotPermission,
			common.ProcedureDeviceInfo,
			common.ProcedureDeviceIsDesktop,
			common.ProcedureProcessList,
			common.ProcedureProcessSignal,
			common.ProcedureAppList,
			common.ProcedureAppIcon,
			common.ProcedureAISessionList,
			common.ProcedureAISessionPull,
			common.ProcedureGitStatus,
			common.ProcedureGitOriginal,
			common.ProcedureFileCat,
		},
		StreamSocket: streamSockPath,
	}
	xlinkDone := make(chan struct{})
	common.SafeGo(func() {
		defer close(xlinkDone)
		if !*standalone {
			cloudConfig, err := newCloudConfig(cfgDirectory)
			if err != nil {
				log.Fatal(err)
			}
			if err := xconnd.Run(ctx, xlinkApp, cloudConfig); err != nil {
				log.Fatal(err)
			}
			return
		}

		// Standalone: serve the device realm over QUIC and/or WebTransport to --public-key
		// holders and --user accounts, with no cloud account.
		if err := xconnd.RunStandalone(ctx, xlinkApp, standaloneConfig); err != nil {
			log.Fatalf("standalone: %v", err)
		}
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	<-sigChan

	cancel()
	<-xlinkDone
}

// newStandaloneConfig serves the standalone device realm over transport (see the transport*
// constants) with the certificate in certFile/keyFile, or else a self-signed one kept in
// cfgDirectory, whose fingerprint clients pin with desk --cert-hash.
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
		"connect with desk --cert-hash %s)", common.CertificateFingerprint(cert), common.CertificateFingerprint(cert))

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

// newCloudConfig is how xlink attaches this device to the deskconn cloud.
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
		LANPort:               lanPort,
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
func registerLocalProcedures(session *xconn.Session, deskconnApis *deskconnd.Deskconn,
	clientSessions *deskconnd.ClientSessions, cfgDirectory string) error {
	handlers := map[string]xconn.InvocationHandler{
		common.ProcedureProxyFileOp:       deskconnd.ProxyFileOpHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyDeviceInfo:   deskconnd.ProxyDeviceInfoHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyPing:         deskconnd.ProxyPingHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyCat:          deskconnd.ProxyCatHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyVPNStart:     deskconnd.ProxyVPNStartHandler(deskconnApis),
		common.ProcedureProxyVPNStop:      deskconnd.ProxyVPNStopHandler(deskconnApis),
		common.ProcedureProxyPrinterList:  deskconnd.ProxyPrinterListHandler(clientSessions, cfgDirectory),
		common.ProcedureProxyPrinterPrint: deskconnd.ProxyPrinterPrintHandler(clientSessions, cfgDirectory),
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
			_, err = clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
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
		common.ProcedureConnectedDevices: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult(clientSessions.DeviceSessions())
		},
	}

	for uri, handler := range handlers {
		if resp := session.Register(uri, handler).Do(); resp.Err != nil {
			return resp.Err
		}
	}
	return nil
}
