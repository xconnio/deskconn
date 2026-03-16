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
	"time"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	port = 8080

	deskconnRealm = "io.xconn.deskconn"
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
				MatchPolicy: "prefix",
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

	regRespShell := sess.Register(deskconn.ProcedureProxyShell, deskconn.ProxyProgressiveInvocationHandler(proxyCalls,
		clientSession, cfgDirectory, deskconn.ProcedureShell)).Do()
	if regRespShell.Err != nil {
		log.Fatal(regRespShell.Err)
	}

	regRespExec := sess.Register(deskconn.ProcedureProxyExec, deskconn.ProxyProgressiveInvocationHandler(proxyCalls,
		clientSession, cfgDirectory, deskconn.ProcedureExec)).Do()
	if regRespExec.Err != nil {
		log.Fatal(regRespShell.Err)
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

	deviceRealm := fmt.Sprintf("io.xconn.deskconn.%s.%s", cred.OrganizationID, machineIDStr)

	err = router.AddRealm(deviceRealm, &xconn.RealmConfig{
		AutoDiscloseCaller: true,
		Roles: []xconn.RealmRole{
			{Name: "owner", Permissions: []xconn.Permission{
				{
					URI:         "io.xconn.",
					MatchPolicy: "prefix",
					AllowCall:   true,
				},
			}},
			{Name: "admin", Permissions: []xconn.Permission{
				{
					URI:         "io.xconn.",
					MatchPolicy: "prefix",
					AllowCall:   true,
				},
			}},
			{Name: "member", Permissions: []xconn.Permission{
				{
					URI:         "io.xconn.",
					MatchPolicy: "prefix",
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
	listener, err := server.ListenAndServeWebSocket(xconn.NetworkTCP, "0.0.0.0:8080")
	if err != nil {
		log.Fatalln(err)
	}
	defer listener.Close()

	localSession, err := xconn.ConnectInMemory(router, deviceRealm)
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

	screen := deskconn.NewScreen(sessionBus, systemBus)
	mpris := deskconn.NewMPRIS(sessionBus)
	deskconnApis := deskconn.NewDeskconn(screen, mpris)

	if err := deskconnApis.Register(localSession); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
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

			cloudSession, err := xconn.ConnectCryptosign(ctx, deskconn.CloudURI(), deviceRealm,
				cred.AuthID, cred.PrivateKey)
			if err != nil {
				if err.Error() == "wamp.error.no_such_realm" {
					select {
					case detachChan <- struct{}{}:
					default:
					}
				}
				log.Printf("failed to connect to cloud, will retry in %v: %v", retryDelay, err)

				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				time.Sleep(retryDelay)
				continue
			}

			log.Println("connected successfully to cloud")

			if err := deskconnApis.Register(cloudSession); err != nil {
				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				log.Printf("failed to register procedures on cloud, will retry in %v: %v", retryDelay, err)
				_ = cloudSession.Leave()
				time.Sleep(retryDelay)
				continue
			}

			webRtcManager := xconnwebrtc.NewWebRTCHandler()
			cfg := &xconnwebrtc.ProviderConfig{
				Session:                     cloudSession,
				ProcedureHandleOffer:        deskconn.ProcedureWebRTCOffer,
				TopicHandleRemoteCandidates: deskconn.TopicAnswererOnCandidate,
				TopicPublishLocalCandidate:  deskconn.TopicOffererOnCandidate,
				Serializer:                  &serializers.CBORSerializer{},
				Authenticator:               authenticator,
				Router:                      router,
			}
			if err := webRtcManager.Setup(cfg); err != nil {
				log.Fatal("Failed to setup webRtc provider:", err)
			}

			// reset backoff after successful connection
			retryDelay = 1 * time.Second

			// wait for session to disconnect
			<-cloudSession.Done()

			log.Println("disconnected from cloud, retrying...")
		}
	}()

	go func() {
		retryDelay := 1 * time.Second
		maxDelay := 30 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			cloudSession, err := xconn.ConnectCryptosign(ctx, deskconn.CloudURI(), deskconnRealm,
				cred.AuthID, cred.PrivateKey)
			if err != nil {
				log.Printf("failed to connect to cloud realm, will retry in %v: %v", retryDelay, err)

				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				time.Sleep(retryDelay)
				continue
			}

			callResp := cloudSession.Call(deskconn.ProcedureListKeys).Do()
			if callResp.Err != nil {
				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				log.Println("Failed to list keys:", callResp.Err)
				_ = cloudSession.Leave()
				time.Sleep(retryDelay)
				continue
			}

			jsonData, err := json.MarshalIndent(callResp.Args()[0], "", "  ")
			if err != nil {
				log.Println(err)
				_ = cloudSession.Leave()
				time.Sleep(retryDelay)
				continue
			}

			var cryptosignPrincipals []*deskconn.CryptosignPrincipal
			if err = json.Unmarshal(jsonData, &cryptosignPrincipals); err != nil {
				log.Println(err)
				_ = cloudSession.Leave()
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
			// reset backoff after successful connection
			retryDelay = 1 * time.Second

			// wait for session to disconnect
			<-cloudSession.Done()

			log.Println("disconnected from cloud, retrying...")
		}
	}()

	zeroconfServer, err := deskconn.AdvertiseService(host, port, deviceRealm, cred.OrganizationID)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	select {
	case <-sigChan:
	case <-detachChan:
		cancel()
		_ = os.Remove(filepath.Join(cfgDirectory, "credentials.json"))
		_ = listener.Close()
		router.Close()
		signal.Stop(sigChan)
		goto start
	}
}
