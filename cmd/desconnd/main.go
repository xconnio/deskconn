package main

import (
	"os"
	"os/signal"

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

	session, err := xconn.ConnectInMemory(router, realm)
	if err != nil {
		log.Fatal(err)
	}

	dbusConn, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Fatal(err)
	}
	defer dbusConn.Close()

	brightness := deskconn.NewBrightness(dbusConn)
	deskconnApis := deskconn.NewDeskconn(session, brightness)

	if err := deskconnApis.Start(); err != nil {
		log.Fatal(err)
	}

	zeroconfServer, err := deskconn.AdvertiseService(host, port, realm)
	if err != nil {
		log.Fatal(err)
	}
	defer zeroconfServer.Shutdown()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
}
