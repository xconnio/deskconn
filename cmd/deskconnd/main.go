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

	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	principalsFile := filepath.Join(cfgDirectory, "principals.json")

	var principals []*deskconn.CryptosignPrincipal

	data, err := os.ReadFile(principalsFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
	} else {
		if err := json.Unmarshal(data, &principals); err != nil {
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

	go func() {
		retryDelay := 1 * time.Second
		maxDelay := 30 * time.Second
		for {
			cloudSession, err := xconn.ConnectCryptosign(context.Background(), deskconn.CloudURI(), deviceRealm,
				cred.AuthID, cred.PrivateKey)
			if err != nil {
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
			cloudSession, err := xconn.ConnectCryptosign(context.Background(), deskconn.CloudURI(), deskconnRealm,
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
			if err = os.WriteFile(principalsFile, jsonData, 0600); err != nil {
				log.Println(err)
			}

			authenticator.SetPrincipals(cryptosignPrincipals)

			// reset backoff after successful connection
			retryDelay = 1 * time.Second

			// wait for session to disconnect
			<-cloudSession.Done()

			log.Println("disconnected from cloud, retrying...")
		}
	}()

	zeroconfServer, err := deskconn.AdvertiseService(host, port, deviceRealm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
}
