package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/xconn-go"
)

func main() {
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

	common.SafeGo(func() { runXlinkSession(ctx, cfgDirectory, deskconnApis, clientSessions) })

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)
	<-sigChan

	cancel()
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

// runXlinkSession keeps a session on xlink's local realm alive, registering
// every app-layer and CLI-facing procedure on it, reconnecting on failure.
func runXlinkSession(ctx context.Context, cfgDirectory string, deskconnApis *deskconnd.Deskconn,
	clientSessions *deskconnd.ClientSessions) {
	retryDelay := 1 * time.Second
	maxDelay := 30 * time.Second

	localSockPath := filepath.Join(cfgDirectory, "deskconn.sock")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		session, err := xconn.ConnectAnonymous(ctx, fmt.Sprintf("unix://%s", localSockPath), common.LocalRealm)
		if err != nil {
			log.Printf("xlink session: failed to connect to xlink, will retry in %v: %v", retryDelay, err)
			retryDelay = min(retryDelay*2, maxDelay)
			time.Sleep(retryDelay)
			continue
		}

		if err := deskconnApis.Register(session); err != nil {
			log.Printf("xlink session: failed to register app-layer procedures, will retry in %v: %v",
				retryDelay, err)
			_ = session.Leave()
			retryDelay = min(retryDelay*2, maxDelay)
			time.Sleep(retryDelay)
			continue
		}

		if err := registerLocalProcedures(session, deskconnApis, clientSessions, cfgDirectory); err != nil {
			log.Printf("xlink session: failed to register CLI-facing procedures, will retry in %v: %v",
				retryDelay, err)
			_ = session.Leave()
			retryDelay = min(retryDelay*2, maxDelay)
			time.Sleep(retryDelay)
			continue
		}

		log.Println("xlink session: registered procedures with xlink")
		retryDelay = 1 * time.Second

		select {
		case <-session.Done():
			log.Println("xlink session: disconnected from xlink, retrying...")
		case <-ctx.Done():
			_ = session.Leave()
			return
		}
	}
}
