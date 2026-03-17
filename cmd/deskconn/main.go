package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

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
	shellDeviceName := shellCmd.Arg("device", "ID, name or alias of device to shell").Required().String()

	execCmd := app.Command("exec", "Run a command")
	execDeviceName := execCmd.Arg("device", "ID, name or alias of device to run command").Required().String()
	command := execCmd.Arg("command", "Command to run").Required().Strings()

	lsCmd := app.Command("ls", "List devices")
	lsRefreshFlag := lsCmd.Flag("refresh", "Refresh device list from cloud").Bool()
	lsDetailedFlag := lsCmd.Flag("detailed", "Show detailed output").Bool()

	whoamiCMD := app.Command("whoami", "Whoami")

	logoutCmd := app.Command("logout", "Logout")

	configCmd := app.Command("config", "Manage deskconn configuration")
	configShow := configCmd.Command("show", "Show config")
	configSet := configCmd.Command("set", "Set device alias")
	configSetDevice := configSet.Arg("device", "ID, name or alias of device").Required().String()
	configSetKey := configSet.Arg("key", "Config key to set").Required().String()
	configSetValue := configSet.Arg("value", "Config value").Required().String()
	configUnset := configCmd.Command("unset", "Unset device alias")
	configUnsetDevice := configUnset.Arg("device", "ID, name or alias of device").Required().String()
	configUnsetKey := configUnset.Arg("key", "Config key").Required().String()
	configEdit := configCmd.Command("edit", "Edit full config")

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
			return
		}

		callResp := session.Call(deskconn.ProcedureLogin).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case shellCmd.FullCommand():
		realm, err := deviceRealm(*shellDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := deskconn.StartInteractiveCommand(session, deskconn.ProcedureProxyShell, realm); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case execCmd.FullCommand():
		realm, err := deviceRealm(*execDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := deskconn.StartInteractiveCommand(session, deskconn.ProcedureProxyExec, realm, *command...); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case lsCmd.FullCommand():
		fileExists := true
		devicesFromCfg, err := deskconn.DevicesFromCfg(cfgDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fileExists = false
			} else {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		}
		tableHeader := []string{"ID", "NAME", "ALIAS", "CONNECTED"}
		if *lsDetailedFlag {
			tableHeader = append(tableHeader, "ORGANIZATION", "REALM")
		}
		if !*lsRefreshFlag && fileExists {
			table := tablewriter.NewWriter(os.Stdout)
			table.Header(tableHeader)
			for _, d := range devicesFromCfg {
				if *lsDetailedFlag {
					_ = table.Append([]any{d.Authid, d.Name, d.Alias, d.Connected, d.Organization.Name, d.Realm})
				} else {
					_ = table.Append([]any{d.Authid, d.Name, d.Alias, d.Connected})
				}
			}

			if err = table.Render(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return
		}

		cloudSession, err := connectCloudRealm(cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		callResp := cloudSession.Call(deskconn.ProcedureListDesktop).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}

		var devices []deskconn.Device
		jsonData, err := json.Marshal(callResp.Args())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := json.Unmarshal(jsonData, &devices); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.Header(tableHeader)

		cfgMap := make(map[string]deskconn.Device, len(devicesFromCfg))
		for _, d := range devicesFromCfg {
			cfgMap[d.Authid] = d
		}

		nameCount := make(map[string]int)
		for i, d := range devices {
			count := nameCount[d.Name]
			if count > 0 {
				newName := fmt.Sprintf("%s-%s", d.Name, d.Authid[:6])
				devices[i].Name = newName
			}
			if localDevice, ok := cfgMap[d.Authid]; ok {
				devices[i].Alias = localDevice.Alias
				devices[i].Connected = localDevice.Connected
			}
			d = devices[i]
			nameCount[d.Name]++
			if *lsDetailedFlag {
				_ = table.Append([]any{d.Authid, d.Name, d.Alias, d.Connected, d.Organization.Name, d.Realm})
			} else {
				_ = table.Append([]any{d.Authid, d.Name, d.Alias, d.Connected})
			}
		}

		if err = table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		b, err := yaml.Marshal(devices)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), b, 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case whoamiCMD.FullCommand():
		authid, _, err := deskconn.ReadCredentials(cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Println(authid)

	case logoutCmd.FullCommand():
		if err := logout(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		callResp := session.Call(deskconn.ProcedureLogout).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case configShow.FullCommand():
		data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Print(string(data))

	case configSet.FullCommand():
		if *configSetKey != "alias" {
			fmt.Fprintln(os.Stderr, "unsupported key to set")
		}
		if err := updateDeviceAlias(cfgDirectory, *configSetDevice, *configSetValue); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case configUnset.FullCommand():
		if *configUnsetKey != "alias" {
			fmt.Fprintln(os.Stderr, "unsupported key to unset")
		}
		if err := updateDeviceAlias(cfgDirectory, *configUnsetDevice, ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case configEdit.FullCommand():
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}

		cmd := exec.Command(editor, filepath.Join(cfgDirectory, "config.yml")) // nolint: gosec
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
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

	authID, orgID, err := selectDevice(session)
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

func logout(cfgDirectory string) error {
	cloudSession, err := connectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}

	pubKey, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519.pub"))
	if err != nil {
		return err
	}
	pubKeyString := strings.Split(string(pubKey), " ")[0]
	callResp := cloudSession.Call(deskconn.ProcedurePrincipalDelete).Arg(pubKeyString).Do()
	if callResp.Err != nil {
		return callResp.Err
	}

	files := []string{
		filepath.Join(cfgDirectory, "id_ed25519"),
		filepath.Join(cfgDirectory, "id_ed25519.pub"),
		filepath.Join(cfgDirectory, "config.yml"),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func deviceRealm(deviceName, cfgDirectory string) (string, error) {
	devices, err := deskconn.DevicesFromCfg(cfgDirectory)
	if err != nil {
		return "", err
	}

	for _, d := range devices {
		if d.Name == deviceName {
			return d.Realm, nil
		}
		if d.Authid == deviceName {
			return d.Realm, nil
		}
		if d.Alias == deviceName {
			return d.Realm, nil
		}
	}

	return "", fmt.Errorf("device not found: %s", deviceName)
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

func selectDevice(session *xconn.Session) (authid string, organizationID string, err error) {
	callResp := session.Call(deskconn.ProcedureListDesktop).Do()

	if callResp.Err != nil {
		return "", "", callResp.Err
	}
	if len(callResp.Args()) == 0 {
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

func updateDeviceAlias(cfgDirectory, deviceKey, alias string) error {
	configFile := filepath.Join(cfgDirectory, "config.yml")

	data, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var devices []deskconn.Device
	if err := yaml.Unmarshal(data, &devices); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	updated := false
	for i, d := range devices {
		if d.Authid == deviceKey || d.Name == deviceKey || d.Alias == deviceKey {
			devices[i].Alias = alias
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("device %s not found", deviceKey)
	}

	out, err := yaml.Marshal(devices)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(configFile, out, 0600)
}

func connectCloudRealm(cfgDirectory string) (*xconn.Session, error) {
	authid, privKey, err := deskconn.ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	return xconn.ConnectCryptosign(context.Background(), deskconn.CloudURI(), deskconn.Realm, authid, privKey)
}
