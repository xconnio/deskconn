package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

var version = "v0.1.0-alpha"

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	versionString := fmt.Sprintf("deskconn %s", version)
	app := kingpin.New("deskconn", "Deskconn control CLI")
	app.Version(versionString)
	app.Command("version", "Show version")

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

	whoamiCMD := app.Command("whoami", "Show current user")

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
	updateCmd := app.Command("update", "Check for and install app updates")

	parsedCmd := kingpin.MustParse(app.Parse(os.Args[1:]))

	var session *xconn.Session
	if parsedCmd != "version" && parsedCmd != updateCmd.FullCommand() {
		uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
		session, err = xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}
	}

	switch parsedCmd {
	case "version":
		fmt.Println(versionString)

	case attachCmd.FullCommand():
		if err := attach(*attachUsername, *attachName, *attachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case detachCmd.FullCommand():
		if err := detach(*detachUsername, *detachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case loginCmd.FullCommand():
		if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
			fmt.Fprintln(os.Stderr, "you are already logged in, please logout first")
			return
		}

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
		devicesCallResp := session.Call(deskconn.ProcedureConnectedDevices).Do()
		if devicesCallResp.Err != nil {
			fmt.Fprintln(os.Stderr, devicesCallResp.Err)
			return
		}
		connectedDevices, ok := devicesCallResp.Args()[0].(map[string]any)
		if !ok {
			fmt.Fprintln(os.Stderr, "expected a map of strings")
			return
		}
		fileExists := true
		devicesFromCfg, err := deskconn.DevicesFromCfg(cfgDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fileExists = false
			} else {
				if !*lsRefreshFlag {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				fmt.Println("your config is invalid, refreshing from cloud")
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
				_, connected := connectedDevices[d.Realm]
				if connected {
					d.Connected = true
				}
				if d.Name == "" && d.Authid == "" && d.Alias == "" {
					continue
				}

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

		devices, err := deskconn.FetchDevicesFromCloud(cfgDirectory)
		if err != nil {
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

		b, err := yaml.Marshal(deskconn.Config{Devices: devices})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), b, 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case whoamiCMD.FullCommand():
		path := filepath.Join(cfgDirectory, "id_ed25519.pub")

		credentialsStr, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "user not logged in")
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		credentials := strings.Split(strings.TrimSpace(string(credentialsStr)), " ")

		if len(credentials) > 2 {
			fmt.Printf("Logged in as %s (%s).\n", credentials[2], credentials[1])
		} else {
			fmt.Printf("Logged in as %s.\n", credentials[1])
		}

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
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "no config found, user not logged in")
				return
			}
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
		_, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "no config found, user not logged in")
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}

		cmd := exec.Command(editor, filepath.Join(cfgDirectory, "config.yml")) // nolint: gosec
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()

	case updateCmd.FullCommand():
		if err := updateApp(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}
}

type appUpdateResponse struct {
	DownloadURL string `json:"download_url"`
}

func updateApp(cfgDirectory string) error {
	cloudSession, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}

	callResp := cloudSession.Call(deskconn.ProcedureAppUpdateCheck).Args("deskconn", version, runtime.GOOS,
		runtime.GOARCH).Do()
	if callResp.Err != nil {
		return callResp.Err
	}

	if len(callResp.Args()) == 0 {
		fmt.Println("No updates available.")
		return nil
	}

	var updateResp appUpdateResponse
	jsonData, err := json.Marshal(callResp.Args()[0])
	if err != nil {
		return fmt.Errorf("failed to marshal update response: %w", err)
	}

	if err := json.Unmarshal(jsonData, &updateResp); err != nil {
		return fmt.Errorf("failed to parse update response: %w", err)
	}

	if updateResp.DownloadURL == "" {
		return fmt.Errorf("update response missing download_url")
	}

	if err := downloadAndInstallUpdate(updateResp.DownloadURL); err != nil {
		return err
	}

	cmd := exec.Command("systemctl", "--user", "restart", "deskconnd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart deskconnd: %w", err)
	}

	fmt.Println("Update installed successfully.")
	return nil
}

func downloadAndInstallUpdate(downloadURL string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download update: unexpected status %s", resp.Status)
	}

	gzipReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to open update archive: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	binDir := filepath.Join(homeDir, ".local", "bin")
	execDir := filepath.Join(homeDir, ".local", "lib", "exec")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin dir: %w", err)
	}
	if err := os.MkdirAll(execDir, 0755); err != nil {
		return fmt.Errorf("failed to create exec dir: %w", err)
	}

	foundDeskconn := false
	foundDeskconnd := false

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read update archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(header.Name)
		switch name {
		case "deskconn":
			if err := installBinaryFromReader(tarReader, filepath.Join(binDir, "deskconn"), 0755); err != nil {
				return err
			}
			foundDeskconn = true
		case "deskconnd":
			if err := installBinaryFromReader(tarReader, filepath.Join(execDir, "deskconnd"), 0700); err != nil {
				return err
			}
			foundDeskconnd = true
		}
	}

	if !foundDeskconn || !foundDeskconnd {
		return fmt.Errorf("update archive missing required binaries")
	}

	return nil
}

func installBinaryFromReader(src io.Reader, dst string, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", dst, err)
	}

	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		_ = tmpFile.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmpFile, src); err != nil {
		return fmt.Errorf("failed to write %s: %w", dst, err)
	}

	if err := tmpFile.Chmod(mode); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", dst, err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", dst, err)
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("failed to install %s: %w", dst, err)
	}

	cleanup = false
	return nil
}

func attach(username, name string, useStdin bool) error {
	file, err := deskconn.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("device already attached")
	}
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

	fmt.Printf("Are you sure you want to detach desktop with ID %s from organization %s?\n(Y/n): ", authID, orgID)

	var confirm string
	_, err = fmt.Scanln(&confirm)
	if err != nil && err.Error() != "unexpected newline" {
		return err
	}

	confirm = strings.TrimSpace(strings.ToLower(confirm))
	if confirm != "" && confirm != "y" && confirm != "yes" {
		fmt.Println("detach cancelled")
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
	cloudSession, err := deskconn.ConnectCloudRealm(cfgDirectory)
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
		if errors.Is(err, os.ErrNotExist) {
			_, err := deskconn.FetchDevicesFromCloud(cfgDirectory)
			if err != nil {
				return "", err
			}

			b, err := yaml.Marshal(deskconn.Config{Devices: devices})
			if err != nil {
				return "", err
			}

			if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), b, 0600); err != nil {
				return "", err
			}

			return deviceRealm(deviceName, cfgDirectory)
		}
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
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config found. User not logged in")
		}
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config deskconn.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	devices := config.Devices
	for _, d := range devices {
		if (d.Alias == alias && d.Alias != "") || d.Authid == alias || d.Name == alias {
			return fmt.Errorf("alias '%s' already in use", alias)
		}
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

	out, err := yaml.Marshal(deskconn.Config{Devices: devices})
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(configFile, out, 0600)
}
