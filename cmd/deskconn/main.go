package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
	session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}

	app := kingpin.New("deskconn", "Deskconn control CLI")

	attachCmd := app.Command("attach", "Attach a device")
	attachName := attachCmd.Flag("name", "Device name").Short('n').String()
	attachPasswordStdin := attachCmd.Flag("password-stdin", "Read password from stdin").Bool()
	attachUsername := attachCmd.Arg("username", "Username").Required().String()

	detachCmd := app.Command("detach", "Detach device")
	detachPasswordStdin := detachCmd.Flag("password-stdin", "Read password from stdin").Bool()
	detachUsername := detachCmd.Arg("username", "Username").Required().String()

	loginCmd := app.Command("login", "Login and store credentials")
	loginPasswordStdin := loginCmd.Flag("password-stdin", "Read password from stdin").Bool()
	loginUsername := loginCmd.Arg("username", "Username").Required().String()

	shellCmd := app.Command("shell", "Start interactive shell")
	shellDeviceName := shellCmd.Arg("name", "Name of device to shell").String()

	execCmd := app.Command("exec", "Run a command")
	command := execCmd.Arg("command", "Command to run").Required().Strings()
	execDeviceName := execCmd.Flag("name", "Name of device to run command").Short('n').String()

	lsCmd := app.Command("ls", "List devices")

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case attachCmd.FullCommand():
		if err := attach(*attachUsername, *attachName, *attachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case detachCmd.FullCommand():
		if err := detach(*detachUsername, *detachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case loginCmd.FullCommand():
		if err := login(*loginUsername, *loginPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case shellCmd.FullCommand():
		realm, err := deviceRealm(session, *shellDeviceName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := deskconn.StartInteractiveCommand(session, deskconn.ProcedureProxyShell, realm); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case execCmd.FullCommand():
		realm, err := deviceRealm(session, *execDeviceName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := deskconn.StartInteractiveCommand(session, deskconn.ProcedureProxyExec, realm, *command...); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case lsCmd.FullCommand():
		callResp := session.Call(deskconn.ProcedureListDesktop).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}

		table := tablewriter.NewWriter(os.Stdout)

		table.Header([]string{"NAME", "ORGANIZATION", "DEVICE ID"})

		for _, d := range callResp.Args() {
			device, ok := d.(map[string]any)
			if !ok {
				continue
			}

			name, _ := device["name"].(string)
			organization, _ := device["organization"].(map[string]any)

			_ = table.Append([]string{name, organization["name"].(string), device["id"].(string)})
		}

		if err = table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func attach(username, name string, useStdin bool) error {
	password, err := readPassword(useStdin)
	if err != nil {
		return err
	}

	deviceName := name

	if deviceName == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}

		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Name of the new device [default=%s]: ", host)

		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			deviceName = host
		} else {
			deviceName = input
		}
	}

	session, err := xconn.ConnectCRA(context.Background(), deskconn.CloudURI(), deskconn.Realm, username, password)
	if err != nil {
		return err
	}

	callResp := session.Call(deskconn.ProcedureDeskconnOrganizationList).Do()
	if callResp.Err != nil {
		return callResp.Err
	}
	var organizationDict xconn.Dict
	if len(callResp.Args()) == 0 {
		fmt.Println("No organization found.")

		reader := bufio.NewReader(os.Stdin)

		var orgName string
		for {
			fmt.Print("Enter organization name to create: ")

			input, err := reader.ReadString('\n')
			if err != nil {
				return err
			}

			orgName = strings.TrimSpace(input)
			if orgName == "" {
				fmt.Println("Organization name cannot be empty.")
				continue
			}

			break
		}

		createResp := session.Call(deskconn.ProcedureOrganizationCreate).Arg(orgName).Do()
		if createResp.Err != nil {
			return createResp.Err
		}

		organizationDict, err = createResp.ArgDict(0)
		if err != nil {
			return err
		}
	} else {
		idx, err := selectOrganization(callResp)
		if err != nil {
			return err
		}

		organizationDict, err = callResp.ArgDict(idx)
		if err != nil {
			return err
		}
	}
	return deskconn.Attach(session, deviceName, organizationDict.StringOr("id", ""))
}

func detach(username string, useStdin bool) error {
	password, err := readPassword(useStdin)
	if err != nil {
		return err
	}

	session, err := xconn.ConnectCRA(context.Background(), deskconn.CloudURI(), deskconn.Realm, username, password)
	if err != nil {
		return err
	}

	authID, orgID, err := selectDevice(session, "")
	if err != nil {
		return err
	}

	fmt.Printf("Are you sure you want to detach desktop with ID %s from organization %s? (y/N): ", authID, orgID)

	var confirm string
	_, err = fmt.Scanln(&confirm)
	if err != nil {
		return err
	}

	if strings.ToLower(confirm) != "y" && strings.ToLower(confirm) != "yes" {
		fmt.Println("Detach cancelled.")
		return nil
	}

	return deskconn.Detach(session, authID)
}

func login(username string, useStdin bool) error {
	password, err := readPassword(useStdin)
	if err != nil {
		return err
	}

	session, err := xconn.ConnectCRA(context.Background(), deskconn.CloudURI(), deskconn.Realm, username, password)
	if err != nil {
		return err
	}

	return deskconn.Login(session, username)
}

func deviceRealm(session *xconn.Session, deviceName string) (string, error) {
	machineID, organizationID, err := selectDevice(session, deviceName)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("io.xconn.deskconn.%s.%s", organizationID, machineID), nil
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin {
		reader := bufio.NewReader(os.Stdin)
		pwd, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimRight(pwd, "\r\n"), nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("password required from TTY or use --password-stdin")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	return string(pwd), nil
}

func selectOption(callResp xconn.CallResponse, title string, idField string, prompt string) (int, error) {
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
		id, _ := dict.String(idField)

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
	fmt.Println(" ", title)
	fmt.Println(sep)

	for _, r := range rows {
		fmt.Println(r.line)
	}

	fmt.Println(sep)
	fmt.Printf(" %s [1-%d] (default 1): ", prompt, count)

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

func selectDevice(session *xconn.Session, deviceName string) (authid string, organizationID string, err error) {
	call := session.Call(deskconn.ProcedureListDesktop)
	if deviceName != "" {
		call.Kwarg("name", deviceName)
	}
	callResp := call.Do()

	if callResp.Err != nil {
		return "", "", callResp.Err
	}
	if len(callResp.Args()) == 0 {
		if deviceName != "" {
			return "", "", fmt.Errorf("no desktop with name %s attached to the account", deviceName)
		}
		return "", "", fmt.Errorf("no desktop attached to the account")
	}

	idx, err := selectOption(callResp, "Available devices", "authid", "Select device")
	if err != nil {
		return "", "", err
	}

	deviceDict, err := callResp.ArgDict(idx)
	if err != nil {
		return "", "", err
	}
	authid, err = deviceDict.String("authid")
	if err != nil {
		return "", "", err
	}
	organizationID, err = deviceDict.String("organization_id")
	if err != nil {
		return "", "", err
	}

	return authid, organizationID, nil
}

func selectOrganization(callResp xconn.CallResponse) (int, error) {
	return selectOption(
		callResp,
		"Available organizations",
		"id",
		"Select organization",
	)
}
