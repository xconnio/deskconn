package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
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
	case "shell":
		if err := shell(os.Args[2:]); err != nil {
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

func shell(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("shell requires <username> <password>")
	}
	username := args[0]
	password := args[1]
	session, err := xconn.ConnectCRA(context.Background(), deskconn.URI, deskconn.Realm, username, password)
	if err != nil {
		return err
	}

	callResp := session.Call("io.xconn.deskconn.desktop.list").Do()
	if callResp.Err != nil {
		return callResp.Err
	}
	if len(callResp.Args()) == 0 {
		return fmt.Errorf("no desktop attached to the account")
	}

	idx, err := selectDevice(callResp)
	if err != nil {
		return err
	}

	deviceDict, err := callResp.ArgDict(idx)
	if err != nil {
		return err
	}

	machineID, err := deviceDict.String("authid")
	if err != nil {
		return err
	}

	return deskconn.StartInteractiveShell(session, fmt.Sprintf(deskconn.ProcedureShellCloud, machineID))
}

func selectDevice(callResp xconn.CallResponse) (int, error) {
	count := len(callResp.Args())
	if count == 1 {
		return 0, nil
	}

	type row struct {
		i    int
		name string
		id   string
		line string
	}

	rows := make([]row, 0, count)
	maxWidth := 0

	for i := 0; i < count; i++ {
		dict, err := callResp.ArgDict(i)
		if err != nil {
			return -1, err
		}

		name, _ := dict.String("name")
		id, _ := dict.String("authid")
		if name == "" {
			name = id
		}

		line := fmt.Sprintf(" %2d) %-20s  %s", i+1, name, id)
		if len(line) > maxWidth {
			maxWidth = len(line)
		}

		rows = append(rows, row{i + 1, name, id, line})
	}

	sep := strings.Repeat("─", maxWidth)

	fmt.Println()
	fmt.Println(sep)
	fmt.Println(" Available devices")
	fmt.Println(sep)

	for _, r := range rows {
		fmt.Println(r.line)
	}

	fmt.Println(sep)
	fmt.Printf(" Select device [1-%d] (default 1): ", count)

	reader := bufio.NewReader(os.Stdin)

	for {
		input, err := reader.ReadString('\n')
		if err != nil {
			return -1, err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			return 0, nil
		}

		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > count {
			fmt.Printf(" Invalid selection. Enter 1-%d: ", count)
			continue
		}

		return idx - 1, nil
	}
}

func usage() {
	fmt.Println(`Usage:
  deskconnctl attach [--name|-n <name>] <username> <password>`)
}
