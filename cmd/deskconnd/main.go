package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

const (
	realm = "realm1"
	port  = 8080
)

func main() {
	cred, err := deskconn.EnsureCredentials()
	if err != nil {
		log.Fatal(err)
	}

	host, _ := os.Hostname()

	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	if err != nil {
		log.Fatalln(err)
	}
	err = router.AddRealm(realm, &xconn.RealmConfig{
		Roles: []xconn.RealmRole{
			{Name: "anonymous", Permissions: []xconn.Permission{
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

	server := xconn.NewServer(router, nil, &xconn.ServerConfig{})
	listener, err := server.ListenAndServeWebSocket(xconn.NetworkTCP, "0.0.0.0:8080")
	if err != nil {
		log.Fatalln(err)
	}
	defer listener.Close()

	localSession, err := xconn.ConnectInMemory(router, realm)
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
	deskconnApis := deskconn.NewDeskconn(screen)

	if err := deskconnApis.RegisterLocal(localSession); err != nil {
		log.Fatal(err)
	}

	go func() {
		machineID, err := os.ReadFile(deskconn.MachineIDPath)
		if err != nil {
			log.Fatalln("failed to read machine-id: ", err)
		}
		machineIDStr := strings.TrimSpace(string(machineID))

		retryDelay := 1 * time.Second
		maxDelay := 30 * time.Second
		for {
			cloudSession, err := xconn.ConnectCryptosign(context.Background(), deskconn.URI, deskconn.Realm,
				cred.AuthID, cred.PrivateKey)
			if err != nil {
				log.Printf("failed to connect to cloud, will retry in %v: %v", retryDelay, err)

				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				continue
			}

			log.Println("connected successfully to cloud")

			// reset backoff after successful connection
			retryDelay = 1 * time.Second

			if err := deskconnApis.RegisterCloud(cloudSession, machineIDStr); err != nil {
				// exponential backoff
				retryDelay *= 2
				if retryDelay > maxDelay {
					retryDelay = maxDelay
				}
				log.Printf("failed to register procedures on cloud, will retry in %v: %v", retryDelay, err)
				_ = cloudSession.Leave()
			}

			// wait for session to disconnect
			<-cloudSession.Done()

			log.Println("disconnected from cloud, retrying...")
		}
	}()

	zeroconfServer, err := deskconn.AdvertiseService(host, port, realm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
}
