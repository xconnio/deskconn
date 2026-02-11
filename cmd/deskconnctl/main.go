package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
	"github.com/xconnio/xconn-go/auth"
	"github.com/xconnio/xconn-webrtc-go"
)

func main() {
	app := kingpin.New("deskconnctl", "Deskconn control CLI")

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
		if err := shell(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func attach(username, name string, useStdin bool) error {
	deviceName := name

	if deviceName == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}
		deviceName = host
	}

	password, err := readPassword(useStdin)
	if err != nil {
		return err
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

	return deskconn.Detach(session)
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

func shell() error {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		return err
	}

	credentialsStr, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519"))
	if err != nil {
		return fmt.Errorf("kindly login first: %w", err)
	}
	credentials := strings.Split(string(credentialsStr), " ")
	authid := strings.TrimSpace(credentials[1])
	privKey := strings.TrimSpace(credentials[0])

	session, err := xconn.ConnectCryptosign(context.Background(), deskconn.CloudURI(), deskconn.Realm, authid, privKey)
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
	organizationID, err := deviceDict.String("organization_id")
	if err != nil {
		return err
	}

	deviceRealm := fmt.Sprintf("io.xconn.deskconn.%s.%s", organizationID, machineID)
	deviceSession, err := xconn.ConnectCryptosign(context.Background(), deskconn.CloudURI(), deviceRealm, authid, privKey)
	if err != nil {
		return err
	}

	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
	if err != nil {
		return err
	}
	config := &xconnwebrtc.ClientConfig{
		Realm:                    deviceRealm,
		ProcedureWebRTCOffer:     deskconn.ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: deskconn.TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  deskconn.TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  deviceSession,
	}

	shellSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		log.Printf("failed to connect using webrtc: %v", err)
		shellSession = deviceSession
	}

	return deskconn.StartInteractiveShell(shellSession)
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

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("password required from TTY or use --password-stdin")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	pwd, err := term.ReadPassword(int(os.Stdin.Fd()))
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

func selectDevice(callResp xconn.CallResponse) (int, error) {
	return selectOption(
		callResp,
		"Available devices",
		"authid",
		"Select device",
	)
}

func selectOrganization(callResp xconn.CallResponse) (int, error) {
	return selectOption(
		callResp,
		"Available organizations",
		"id",
		"Select organization",
	)
}
