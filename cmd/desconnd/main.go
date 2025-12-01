package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

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
	url := fmt.Sprintf("ws://0.0.0.0:%d/ws", port)

	session, err := xconn.ConnectAnonymous(context.Background(), url, realm)
	if err != nil {
		log.Fatal(err)
	}

	brightness := deskconn.NewBrightness()
	deskconndApis := deskconn.NewDeskconnd(session, brightness)

	if err := deskconndApis.Start(); err != nil {
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
