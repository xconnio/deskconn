package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/xconnio/deskconn"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "attach":
		if err := attach(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func attach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)

	name := fs.String("name", "", "")
	fs.StringVar(name, "n", "", "")

	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("attach requires <username> <password>")
	}

	username := rest[0]
	password := rest[1]

	deviceName := *name
	if deviceName == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}
		deviceName = host
	}
	return deskconn.Attach(context.Background(), username, password, deviceName)
}

func usage() {
	fmt.Println(`Usage:
  deskconnctl attach [--name|-n <name>] <username> <password>`)
}
