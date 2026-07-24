package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	port = 18080

	xconnURIPrefix = "io.xconn."
)

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	localRouter, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}

	if err := localRouter.AddRealm(deskconn.LocalRealm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Roles: []xconn.RealmRole{{
			Name: "anonymous",
			Permissions: []xconn.Permission{{
				URI:         "",
				MatchPolicy: wampproto.MatchPrefix,
				AllowCall:   true,
			}},
		}},
	}); err != nil {
		log.Fatalln(err)
	}

	localserver := xconn.NewServer(localRouter, nil, &xconn.ServerConfig{})
	localListener, err := localserver.ListenAndServeRawSocket(xconn.NetworkUnix,
		filepath.Join(cfgDirectory, "deskconn.sock"))
	if err != nil {
		log.Fatalln(err)
	}
	defer localListener.Close()

	sess, err := xconn.ConnectInMemory(localRouter, deskconn.LocalRealm)
	if err != nil {
		log.Fatalln(err)
	}

	proxyCalls := deskconn.NewProxyCalls()
	clientSession := deskconn.NewClientSessions()

	regRespShell := sess.Register(deskconn.ProcedureProxyShell, deskconn.ProxyShellHandler(proxyCalls,
		clientSession, cfgDirectory)).Do()
	if regRespShell.Err != nil {
		log.Fatal(regRespShell.Err)
	}

	regRespExec := sess.Register(deskconn.ProcedureProxyExec, deskconn.ProxyProgressiveInvocationHandler(proxyCalls,
		clientSession, cfgDirectory, deskconn.ProcedureExec)).Do()
	if regRespExec.Err != nil {
		log.Fatal(regRespExec.Err)
	}

	regRespFileOp := sess.Register(deskconn.ProcedureProxyFileOp,
		deskconn.ProxyFileOpHandler(clientSession, cfgDirectory)).Do()
	if regRespFileOp.Err != nil {
		log.Fatal(regRespFileOp.Err)
	}

	regRespDeviceInfo := sess.Register(deskconn.ProcedureProxyDeviceInfo,
		deskconn.ProxyDeviceInfoHandler(clientSession, cfgDirectory)).Do()
	if regRespDeviceInfo.Err != nil {
		log.Fatal(regRespDeviceInfo.Err)
	}

	regRespLogs := sess.Register(deskconn.ProcedureProxyLogs,
		deskconn.ProxyLogsHandler(proxyCalls, clientSession, cfgDirectory)).Do()
	if regRespLogs.Err != nil {
		log.Fatal(regRespLogs.Err)
	}

	regRespPing := sess.Register(deskconn.ProcedureProxyPing,
		deskconn.ProxyPingHandler(clientSession, cfgDirectory)).Do()
	if regRespPing.Err != nil {
		log.Fatal(regRespPing.Err)
	}

	regRespCat := sess.Register(deskconn.ProcedureProxyCat,
		deskconn.ProxyCatHandler(clientSession, cfgDirectory)).Do()
	if regRespCat.Err != nil {
		log.Fatal(regRespCat.Err)
	}

	regRespFilePush := sess.Register(deskconn.ProcedureProxyFilePush,
		deskconn.ProxyProgressiveInvocationHandler(proxyCalls, clientSession, cfgDirectory,
			deskconn.ProcedureFileUpload)).Do()
	if regRespFilePush.Err != nil {
		log.Fatal(regRespFilePush.Err)
	}

	regRespFilePull := sess.Register(deskconn.ProcedureProxyFilePull,
		deskconn.ProxyFilePullHandler(clientSession, cfgDirectory)).Do()
	if regRespFilePull.Err != nil {
		log.Fatal(regRespFilePull.Err)
	}

	regRespPortForward := sess.Register(deskconn.ProcedureProxyPortForward,
		deskconn.ProxyPortForwardHandler(clientSession, cfgDirectory)).Do()
	if regRespPortForward.Err != nil {
		log.Fatal(regRespPortForward.Err)
	}

	regRespPortReverse := sess.Register(deskconn.ProcedureProxyPortReverse,
		deskconn.ProxyPortReverseHandler(clientSession, cfgDirectory)).Do()
	if regRespPortReverse.Err != nil {
		log.Fatal(regRespPortReverse.Err)
	}

	regRespPrinterList := sess.Register(deskconn.ProcedureProxyPrinterList,
		deskconn.ProxyPrinterListHandler(clientSession, cfgDirectory)).Do()
	if regRespPrinterList.Err != nil {
		log.Fatal(regRespPrinterList.Err)
	}

	regRespPrinterPrint := sess.Register(deskconn.ProcedureProxyPrinterPrint,
		deskconn.ProxyPrinterPrintHandler(clientSession, cfgDirectory)).Do()
	if regRespPrinterPrint.Err != nil {
		log.Fatal(regRespPrinterPrint.Err)
	}

	regRespLogin := sess.Register(deskconn.ProcedureLogin,
		func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSession.Login()
			return xconn.NewInvocationResult()
		}).Do()
	if regRespLogin.Err != nil {
		log.Fatal(regRespLogin.Err)
	}

	regRespLogout := sess.Register(deskconn.ProcedureLogout,
		func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSession.Logout()
			return xconn.NewInvocationResult()
		}).Do()
	if regRespLogout.Err != nil {
		log.Fatal(regRespLogout.Err)
	}

	regRespConnect := sess.Register(deskconn.ProcedureConnect,
		func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(deskconn.ErrInvalidArgument, err.Error())
			}
			_, err = clientSession.EnsureP2PDeviceSession(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(deskconn.ErrOperationFailed, err.Error())
			}
			return xconn.NewInvocationResult()
		}).Do()
	if regRespConnect.Err != nil {
		log.Fatal(regRespConnect.Err)
	}

	regRespDisconnect := sess.Register(deskconn.ProcedureDisconnect,
		func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(deskconn.ErrInvalidArgument, err.Error())
			}
			clientSession.Disconnect(realm)
			return xconn.NewInvocationResult()
		}).Do()
	if regRespDisconnect.Err != nil {
		log.Fatal(regRespDisconnect.Err)
	}

	regRespDisconnectAll := sess.Register(deskconn.ProcedureDisconnectAll,
		func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			clientSession.DisconnectAll()
			return xconn.NewInvocationResult()
		}).Do()
	if regRespDisconnectAll.Err != nil {
		log.Fatal(regRespDisconnectAll.Err)
	}

	regRespConnectedDevices := sess.Register(deskconn.ProcedureConnectedDevices,
		func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult(clientSession.DeviceSessions())
		}).Do()
	if regRespConnectedDevices.Err != nil {
		log.Fatal(regRespConnectedDevices.Err)
	}

start:
	cred, err := deskconn.EnsureCredentials()
	if err != nil {
		log.Fatal(err)
	}

	host, _ := os.Hostname()

	machineID, err := os.ReadFile(deskconn.MachineIDPath)
	if err != nil {
		log.Fatalln("failed to read machine-id: ", err)
	}
	machineIDStr := strings.TrimSpace(string(machineID))

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}

	err = router.AddRealm(cred.Realm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Meta:               true,
		Roles: []xconn.RealmRole{
			{Name: "owner", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
			{Name: "admin", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
			{Name: "member", Permissions: []xconn.Permission{
				{
					URI:         xconnURIPrefix,
					MatchPolicy: wampproto.MatchPrefix,
					AllowCall:   true,
				},
			}},
		},
	})
	if err != nil {
		log.Fatalln(err)
	}

	principals, err := deskconn.ReadPrincipalsFromFile()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
	}

	authenticator := deskconn.NewAuthenticator(principals)
	server := xconn.NewServer(router, authenticator, &xconn.ServerConfig{})
	listener, err := server.ListenAndServeWebSocket(xconn.NetworkTCP, "0.0.0.0:18080")
	if err != nil {
		log.Fatalln(err)
	}
	defer listener.Close()

	localSession, err := xconn.ConnectInMemory(router, cred.Realm)
	if err != nil {
		log.Fatal(err)
	}

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

	screen := deskconn.NewScreen(sessionBus, systemBus, cfgDirectory)
	mpris := deskconn.NewMPRIS(sessionBus)
	audio := deskconn.NewAudio()
	defer audio.Close()
	deskconnApis := deskconn.NewDeskconn(screen, mpris, audio)

	if err := deskconnApis.Register(localSession); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	deskconnApis.StartIndexer(ctx)
	defer cancel()

	detachChan := make(chan struct{}, 1)

	go func() {
		retryDelay := 1 * time.Second
		maxDelay := 30 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			cryptosignAuth, err := auth.NewCryptoSignAuthenticator(cred.AuthID, cred.PrivateKey, nil)
			if err != nil {
				log.Printf("failed to initialize cryptosign authenticator: %v", err)
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Open the QUIC connection and the first WAMP session on the device realm.
			deviceSess, err := xconn.ConnectQUIC(ctx, deskconn.CloudQUICAddress(), cred.Realm,
				&xconn.QUICDialerConfig{Authenticator: cryptosignAuth, TLSConfig: deskconn.CloudQUICTLSConfig()})
			if err != nil {
				if err.Error() == "wamp.error.no_such_realm" {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}
				log.Printf("failed to connect to cloud, will retry in %v: %v", retryDelay, err)
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Open a second WAMP session on the cloud realm over the same QUIC connection.
			cloudSess, err := deviceSess.OpenSession(ctx, deskconn.CloudRealm,
				&xconn.QUICDialerConfig{Authenticator: cryptosignAuth})
			if err != nil {
				log.Printf("failed to open cloud realm session, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			deviceSession := deviceSess.Session
			cloudSession := cloudSess.Session

			log.Println("connected to cloud")

			// Accept file-transfer streams relayed from CLI clients.
			go deskconnApis.AcceptQUICStreams(deviceSess)

			if err := deskconnApis.Register(deviceSession); err != nil {
				log.Printf("failed to register procedures on cloud, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			// Fetch and maintain authorized principals via the cloud realm session.
			callResp := cloudSession.Call(deskconn.ProcedureListKeys).Do()
			if callResp.Err != nil {
				log.Println("failed to list keys:", callResp.Err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			jsonData, err := json.MarshalIndent(callResp.Args()[0], "", "  ")
			if err != nil {
				log.Println(err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			var cryptosignPrincipals []*deskconn.CryptosignPrincipal
			if err = json.Unmarshal(jsonData, &cryptosignPrincipals); err != nil {
				log.Println(err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			jsonData = append(jsonData, '\n')
			if err = os.WriteFile(filepath.Join(cfgDirectory, "principals.json"), jsonData, 0600); err != nil {
				log.Println(err)
			}

			authenticator.SetPrincipals(cryptosignPrincipals)
			if err := authenticator.SubscribeEvents(cloudSession, machineIDStr); err != nil {
				log.Println(err)
			}

			subResp := cloudSession.Subscribe(fmt.Sprintf(deskconn.TopicDeskconnDesktopDetachFormat, machineIDStr),
				func(event *xconn.Event) {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}).Do()
			if subResp.Err != nil {
				log.Println(subResp.Err)
			}

			// Fetch TURN credentials and set up WebRTC via the cloud realm session.
			iceServers, expiresAt, err := deskconn.FetchTURNServers(cloudSession)
			if err != nil {
				log.Printf("failed to fetch TURN credentials, using STUN only: %v", err)
				iceServers = []xconnwebrtc.ICEServer{
					{URLs: []string{deskconn.StunServerURL}},
				}
			}

			webRtcManager := xconnwebrtc.NewWebRTCHandler()
			cfg := &xconnwebrtc.ProviderConfig{
				Session:                     deviceSession,
				ProcedureHandleOffer:        deskconn.ProcedureWebRTCOffer,
				TopicHandleRemoteCandidates: deskconn.TopicAnswererOnCandidate,
				TopicPublishLocalCandidate:  deskconn.TopicOffererOnCandidate,
				Serializer:                  &serializers.CBORSerializer{},
				Authenticator:               authenticator,
				Router:                      router,
				ICEServers:                  iceServers,
			}
			if err := webRtcManager.Setup(cfg); err != nil {
				log.Printf("failed to setup webRtc provider, will retry in %v: %v", retryDelay, err)
				_ = deviceSess.Connection().Close()
				retryDelay = min(retryDelay*2, maxDelay)
				time.Sleep(retryDelay)
				continue
			}

			webRtcManager.OnDataChannel(deskconnApis.HandleFileStreamChannel)

			go func(initialExpiresAt int64) {
				currentExpiresAt := initialExpiresAt
				const turnCredentialRefreshBuffer = 5 * time.Minute
				for {
					sleepDur := time.Until(time.Unix(currentExpiresAt, 0)) - turnCredentialRefreshBuffer
					if sleepDur <= 0 {
						sleepDur = 0
					}
					select {
					case <-cloudSession.Done():
						return
					case <-time.After(sleepDur):
					}

					newServers, newExpiresAt, err := deskconn.FetchTURNServers(cloudSession)
					if err != nil {
						log.Printf("failed to refresh TURN credentials: %v", err)
						currentExpiresAt = time.Now().Add(10 * time.Second).Unix()
						continue
					}
					webRtcManager.UpdateICEServers(newServers)
					currentExpiresAt = newExpiresAt
				}
			}(expiresAt)

			// Reset backoff after successful connection.
			retryDelay = 1 * time.Second

			// Both sessions share the QUIC connection; either ending means reconnect.
			select {
			case <-deviceSession.Done():
			case <-cloudSession.Done():
			}

			_ = deviceSess.Connection().Close()
			log.Println("disconnected from cloud, retrying...")
		}
	}()

	zeroconfServer, err := deskconn.AdvertiseService(host, port, cred.Realm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		cancel()
		clientSession.Logout()
		router.Close()
		localRouter.Close()
	case <-detachChan:
		cancel()
		_ = os.Remove(filepath.Join(cfgDirectory, "credentials.json"))
		_ = listener.Close()
		router.Close()
		signal.Stop(sigChan)
		goto start
	}
}
