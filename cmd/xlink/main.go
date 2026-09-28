package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/xlink"
)

// xlink on its own: the device router on a URL, for clients holding the given keys.
// deskconnd embeds the full xlink (cloud and LAN) via xlink.Run.
func main() {
	app := kingpin.New("xlink", "Serve the xlink device router on a URL to clients holding the given keys")
	rawURL := app.Flag("url", "Where to listen: ws://host:port/path, rs://host:port, "+
		"unix:///path/to.sock or unix+ws:///path/to.sock").Required().String()
	keys := app.Flag("public-key", "Public key (hex) allowed to connect; repeat for more").Required().Strings()
	realm := app.Flag("realm", "Realm to serve").Default(common.StandaloneRealm).String()
	kingpin.MustParse(app.Parse(os.Args[1:]))

	addr, stop, err := xlink.ServeRouter(*rawURL, *realm, *keys)
	if err != nil {
		log.Fatal(err)
	}
	defer stop()
	log.Printf("xlink: serving realm %s on %s (%s)", *realm, *rawURL, addr)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan
}
