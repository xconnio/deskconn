package main

import (
	"context"
	"os"
	"os/signal"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconnd"
	"github.com/xconnio/xconn-go"
)

func main() {
	session, err := xconn.ConnectAnonymous(context.Background(), "ws://localhost:8080/ws", "realm1")
	if err != nil {
		log.Fatal(err)
	}

	brightness := deskconnd.NewBrightness()
	deskconndApis := deskconnd.NewDeskconnd(session, brightness)

	if err := deskconndApis.Start(); err != nil {
		log.Fatal(err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
}
