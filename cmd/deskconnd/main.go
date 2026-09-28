package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/deskconn/xlink"
	"github.com/xconnio/xconn-go"
)

func main() {
	app := kingpin.New("deskconnd", "Deskconn daemon: the device's APIs and its connectivity (xlink)")
	standalone := app.Flag("standalone", "Serve this device directly on --url to --public-key holders, "+
		"instead of through the cloud").Bool()
	standaloneURL := app.Flag("url", "Where to listen in standalone mode: tcp://host:port or unix:///path").
		Default("tcp://0.0.0.0:18080").String()
	standaloneKeys := app.Flag("public-key", "Public key (hex) allowed to connect in standalone mode; repeat for more "+
		"(see `deskconn keygen`)").Strings()
	kingpin.MustParse(app.Parse(os.Args[1:]))
	if *standalone && len(*standaloneKeys) == 0 {
		app.Fatalf("--standalone needs at least one --public-key")
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

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
	// remote stream/channel (see deskconn.RelayHeader).
	streamSockPath := filepath.Join(cfgDirectory, "xlink-streams.sock")
	_ = os.Remove(streamSockPath)
	streamListener, err := net.Listen("unix", streamSockPath)
	if err != nil {
		log.Fatal(err)
	}
	defer streamListener.Close()
	common.SafeGo(func() { deskconnApis.ServeStreamRelay(streamListener) })

	// xlink runs in this process: its app layer serves deskconn.sock (the CLI's local
	// realm), and deskconnd registers every app-layer and CLI-facing procedure on it
	// through an in-memory session.
	appRouter, appListener, appSession := xlink.StartAppLayer(cfgDirectory)
	defer appRouter.Close()
	defer appListener.Close()

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

	xlinkDone := make(chan struct{})
	common.SafeGo(func() {
		defer close(xlinkDone)
		if !*standalone {
			xlink.Run(ctx, cfgDirectory, appSession)
			return
		}

		// Standalone: serve the device realm on --url over yamux to --public-key holders, with no
		// cloud account. Raw streams and WebRTC data channels are relayed to streamSockPath.
		router := xlink.NewDeviceRouter(common.StandaloneRealm)
		defer router.Close()
		authenticator := xlink.NewKeyAuthenticator(*standaloneKeys)
		listener, err := common.ListenYamux(*standaloneURL, router, authenticator)
		if err != nil {
			log.Fatalf("standalone: %v", err)
		}
		defer listener.Close()

		localSession, err := xconn.ConnectInMemory(router, common.StandaloneRealm)
		if err != nil {
			log.Fatalf("standalone: %v", err)
		}
		if err := xlink.RegisterBridge(localSession, appSession); err != nil {
			log.Fatalf("standalone: %v", err)
		}
		if err := xlink.SetupWebRTC(localSession, router, authenticator, streamSockPath); err != nil {
			log.Fatalf("standalone: %v", err)
		}
		log.Printf("standalone mode: serving realm %s on %s (%s), %d key(s) authorized",
			common.StandaloneRealm, *standaloneURL, listener.Addr(), len(*standaloneKeys))

		for {
			select {
			case stream := <-listener.Streams():
				common.SafeGo(func() { xlink.RelayQUICStream(stream.Conn, streamSockPath) })
			case <-ctx.Done():
				return
			}
		}
	})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	<-sigChan

	cancel()
	<-xlinkDone
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
