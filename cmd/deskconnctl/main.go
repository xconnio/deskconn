package main

import (
	"fmt"
	"os"

	"github.com/mdp/qrterminal/v3"

	"github.com/xconnio/deskconn"
)

func usage() {
	fmt.Println("deskconnctl pair")
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	if os.Args[1] != "pair" {
		usage()
	}

	pm := deskconn.NewPairManager()
	code := pm.StartPairing()

	url := "deskconn://pair?code=" + code

	fmt.Println("Scan QR from mobile app:")
	qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	fmt.Println("Pair URL:", url)
}
