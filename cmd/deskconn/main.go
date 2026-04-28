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
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/template"
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

const printerUsageTemplate = `{{define "FormatArg" -}}
{{if not .Hidden}} {{if not .Required}}[{{end -}}
{{- if .PlaceHolder}}{{.PlaceHolder}}{{else}}<{{.Name}}>{{end -}}
{{- if .Value|IsCumulative}}...{{end -}}
{{- if not .Required}}]{{end}}{{end -}}
{{end -}}

{{define "FormatCommand" -}}
{{if .FlagSummary}} {{.FlagSummary}}{{end -}}
{{range .Args}}{{template "FormatArg" .}}{{end -}}
{{end -}}

{{define "FormatRestArgs" -}}
{{$skip := true -}}
{{range .Args -}}
{{- if $skip}}{{$skip = false}}{{else}}{{template "FormatArg" .}}{{end -}}
{{- end -}}
{{end -}}

{{define "FormatCommands" -}}
{{range .FlattenedCommands -}}
{{if not .Hidden -}}
{{if isDeviceFirstCmd .FullCommand -}}
  {{printerCmdUsage .FullCommand -}}
{{- template "FormatRestArgs" .}}{{if .FlagSummary}} {{.FlagSummary}}{{end}}
{{else -}}
  {{.FullCommand}}{{if .Default}}*{{end}}{{template "FormatCommand" .}}
{{end -}}
{{.Help|Wrap 4}}
{{end -}}
{{end -}}
{{end -}}

{{define "FormatUsage" -}}
{{template "FormatCommand" .}}{{if .Commands}} <command> [<args> ...]{{end}}
{{if .Help}}
{{.Help|Wrap 0 -}}
{{end -}}

{{end -}}

{{if .Context.SelectedCommand -}}
{{if isDeviceFirstCmd .Context.SelectedCommand.FullCommand -}}
usage: {{.App.Name}} {{printerCmdUsage .Context.SelectedCommand.FullCommand -}}
{{- template "FormatRestArgs" .Context.SelectedCommand -}}
{{- if .Context.SelectedCommand.FlagSummary}} {{.Context.SelectedCommand.FlagSummary}}{{end}}
{{if .Context.SelectedCommand.Help}}
{{.Context.SelectedCommand.Help|Wrap 0 -}}
{{end}}
{{else -}}
usage: {{.App.Name}} {{.Context.SelectedCommand}}{{template "FormatUsage" .Context.SelectedCommand}}
{{end -}}
{{ else -}}
usage: {{.App.Name}}{{template "FormatUsage" .App}}
{{end}}
{{if .Context.Flags -}}
Flags:
{{.Context.Flags|FlagsToTwoColumns|FormatTwoColumns}}
{{end -}}
{{if .Context.Args -}}
Args:
{{.Context.Args|ArgsToTwoColumns|FormatTwoColumns}}
{{end -}}
{{if .Context.SelectedCommand -}}
{{if len .Context.SelectedCommand.Commands -}}
Subcommands:
{{template "FormatCommands" .Context.SelectedCommand}}
{{end -}}
{{else if .App.Commands -}}
Commands:
{{template "FormatCommands" .App}}
{{end -}}
`

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	versionString := fmt.Sprintf("deskconn %s", version)
	app := kingpin.New("deskconn", "Deskconn control CLI")
	app.UsageFuncs(template.FuncMap{
		"isDeviceFirstCmd": func(fullCmd string) bool {
			return fullCmd == "printer list" || fullCmd == "printer print" ||
				fullCmd == "file pull" || fullCmd == "file push"
		},
		"printerCmdUsage": func(fullCmd string) string {
			parts := strings.Fields(fullCmd)
			if len(parts) >= 2 {
				parent := strings.Join(parts[:len(parts)-1], " ")
				return parent + " <device> " + parts[len(parts)-1]
			}
			return fullCmd
		},
	})
	app.UsageTemplate(printerUsageTemplate)

	attachCmd := app.Command("attach", "Attach a device")
	attachName := attachCmd.Flag("name", "Device name").Short('n').String()
	attachUsername := attachCmd.Flag("username", "Username").Short('u').String()
	attachPassword := attachCmd.Flag("password", "Password").Short('p').String()
	attachPasswordStdin := attachCmd.Flag("password-stdin", "Read password from stdin").Bool()

	detachCmd := app.Command("detach", "Detach device")
	detachUsername := detachCmd.Flag("username", "Username").Short('u').String()
	detachPassword := detachCmd.Flag("password", "Password").Short('p').String()
	detachPasswordStdin := detachCmd.Flag("password-stdin", "Read password from stdin").Bool()

	loginCmd := app.Command("login", "Login and store credentials")
	loginUsername := loginCmd.Flag("username", "Username").Short('u').String()
	loginPassword := loginCmd.Flag("password", "Password").Short('p').String()
	loginPasswordStdin := loginCmd.Flag("password-stdin", "Read password from stdin").Bool()

	fileCmd := app.Command("file", "File operations")
	pullCmd := fileCmd.Command("pull", "Download a file or directory from a device")
	pullDevice := pullCmd.Arg("device", "ID, name or alias of device").Required().String()
	pullRemote := pullCmd.Arg("remote-path", "Path on the remote device").Required().String()
	pullLocal := pullCmd.Arg("local-path", "Local path to store the download").Required().String()
	pullRecursive := pullCmd.Flag("recursive", "Download directories recursively").Short('r').Bool()
	pullP2PFlag := pullCmd.Flag("p2p", "Connect using WebRTC").Bool()

	pushCmd := fileCmd.Command("push", "Upload a file or directory to a device")
	pushDevice := pushCmd.Arg("device", "ID, name or alias of device").Required().String()
	pushLocal := pushCmd.Arg("local-path", "Local path to upload").Required().String()
	pushRemote := pushCmd.Arg("remote-path", "Path on the remote device").Required().String()
	pushRecursive := pushCmd.Flag("recursive", "Upload directories recursively").Short('r').Bool()
	pushP2PFlag := pushCmd.Flag("p2p", "Connect using WebRTC").Bool()

	shellCmd := app.Command("shell", "Start interactive shell")
	shellDeviceName := shellCmd.Arg("device", "ID, name or alias of device to shell").Required().String()
	shellP2PFlag := shellCmd.Flag("p2p", "Connect using WebRTC").Bool()

	execCmd := app.Command("exec", "Run a command")
	execDeviceName := execCmd.Arg("device", "ID, name or alias of device to run command").Required().String()
	command := execCmd.Arg("command", "Command to run").Required().Strings()
	execP2PFlag := execCmd.Flag("p2p", "Connect using WebRTC").Bool()

	printerCmd := app.Command("printer", "Printer operations")
	printerEnableCmd := printerCmd.Command("enable", "Enable receiving print jobs on this desktop")
	printerEnableHostPrinters := printerEnableCmd.Flag("host-printers",
		"Also allow remote clients to list this desktop's printers").Bool()
	printerDisableCmd := printerCmd.Command("disable", "Disable receiving print jobs on this desktop")
	printerStatusCmd := printerCmd.Command("status", "Show whether this desktop accepts print jobs")
	printerListCmd := printerCmd.Command("list", "List printers on a device")
	printerListDevice := printerListCmd.Arg("device", "ID, name or alias of device").Required().String()
	printerListP2PFlag := printerListCmd.Flag("p2p", "Connect using WebRTC").Bool()
	printerPrintCmd := printerCmd.Command("print", "Print a local file on a device")
	printerPrintDevice := printerPrintCmd.Arg("device", "ID, name or alias of device").Required().String()
	printerPrintFilePath := printerPrintCmd.Arg("file_path", "Local file path").Required().String()
	printerPrintPrinter := printerPrintCmd.Flag("printer", "Printer name").Required().String()
	printerPrintP2PFlag := printerPrintCmd.Flag("p2p", "Connect using WebRTC").Bool()

	portCmd := app.Command("port", "Port forwarding operations")
	portForwardCmd := portCmd.Command("forward", "Forward a local port to a port on the remote device")
	portForwardDevice := portForwardCmd.Arg("device", "ID, name or alias of device").Required().String()
	portForwardPorts := portForwardCmd.Arg("ports", "Port mapping as localport:remoteport").String()
	portForwardLocalFlag := portForwardCmd.Flag("local", "Local port to listen on").Short('l').String()
	portForwardRemoteFlag := portForwardCmd.Flag("remote", "Port on the remote device to connect to").Short('r').String()
	portForwardP2PFlag := portForwardCmd.Flag("p2p", "Connect using WebRTC").Bool()

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
	selfCmd := app.Command("self", "Manage the installed deskconn CLI.")
	selfVersionCmd := selfCmd.Command("version", "Show the installed deskconn version")
	selfUpdateCmd := selfCmd.Command("update", "Check for updates and install the latest release")

	if len(os.Args) == 2 && os.Args[1] == "self" {
		app.Usage([]string{"self"})
		return
	}

	// Rewrite "printer <device> <subcmd> ..." → "printer <subcmd> <device> ..."
	// so that kingpin sees device as a subcommand arg, not a subcommand name.
	if len(os.Args) > 3 && os.Args[1] == "printer" {
		printerLocalSubs := map[string]bool{"enable": true, "disable": true, "status": true}
		if !printerLocalSubs[os.Args[2]] {
			rewritten := make([]string, len(os.Args))
			copy(rewritten, os.Args)
			rewritten[2], rewritten[3] = rewritten[3], rewritten[2]
			os.Args = rewritten
		}
	}

	// Rewrite "file <device> <subcmd> ..." → "file <subcmd> <device> ..."
	if len(os.Args) > 3 && os.Args[1] == "file" {
		fileSubcmds := map[string]bool{"pull": true, "push": true}
		if !fileSubcmds[os.Args[2]] {
			rewritten := make([]string, len(os.Args))
			copy(rewritten, os.Args)
			rewritten[2], rewritten[3] = rewritten[3], rewritten[2]
			os.Args = rewritten
		}
	}

	parsedCmd := kingpin.MustParse(app.Parse(os.Args[1:]))

	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)

	switch parsedCmd {
	case selfVersionCmd.FullCommand():
		fmt.Println(versionString)

	case attachCmd.FullCommand():
		if err := attach(*attachUsername, *attachPassword, *attachName, *attachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case detachCmd.FullCommand():
		if err := detach(*detachUsername, *detachPassword, *detachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case loginCmd.FullCommand():
		if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
			fmt.Fprintln(os.Stderr, "you are already logged in, please logout first")
			return
		}

		if err := login(*loginUsername, *loginPassword, *loginPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}
		callResp := session.Call(deskconn.ProcedureLogin).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case pullCmd.FullCommand():
		realm, err := deviceRealm(*pullDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *pullP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := deskconn.PullFiles(deviceSession, *pullRemote, *pullLocal, *pullRecursive); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case pushCmd.FullCommand():
		realm, err := deviceRealm(*pushDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *pushP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := deskconn.PushFiles(deviceSession, *pushLocal, *pushRemote, *pushRecursive); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case shellCmd.FullCommand():
		realm, err := deviceRealm(*shellDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *shellP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := deskconn.StartInteractiveCommand(deviceSession, deskconn.ProcedureShell); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case execCmd.FullCommand():
		realm, err := deviceRealm(*execDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *execP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		err = deskconn.StartInteractiveCommand(deviceSession, deskconn.ProcedureExec, *command...)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case printerEnableCmd.FullCommand():
		if *printerEnableHostPrinters {
			if err := deskconn.EnablePrinterHosting(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			fmt.Println("printer hosting enabled")
		} else {
			if err := deskconn.EnablePrinting(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			fmt.Println("printing enabled")
		}

	case printerDisableCmd.FullCommand():
		if err := deskconn.DisablePrinting(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("printing disabled")

	case printerStatusCmd.FullCommand():
		mode, err := deskconn.CurrentPrintMode()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		switch mode {
		case deskconn.PrintModeDisabled:
			fmt.Println("printing disabled")
		case deskconn.PrintModeAccept:
			fmt.Println("printing enabled")
		case deskconn.PrintModeHost:
			fmt.Println("printing enabled; printer hosting enabled")
		default:
			fmt.Printf("printing mode: %s\n", mode)
		}

	case printerListCmd.FullCommand():
		realm, err := deviceRealm(*printerListDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *printerListP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		callResp := deviceSession.Call(deskconn.ProcedurePrinterList).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		if len(callResp.Args()) == 0 {
			return
		}

		jsonData, err := json.Marshal(callResp.Args()[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		var printers []deskconn.PrinterInfo
		if err := json.Unmarshal(jsonData, &printers); err != nil {
			fmt.Fprintln(os.Stderr, "expected a list of printers")
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.Header([]string{"NAME", "PPD"})
		for _, printerInfo := range printers {
			_ = table.Append([]any{printerInfo.Name, printerInfo.PPDModel})
		}
		if err = table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case printerPrintCmd.FullCommand():
		realm, err := deviceRealm(*printerPrintDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		data, err := os.ReadFile(*printerPrintFilePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		filename := filepath.Base(*printerPrintFilePath)

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *printerPrintP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		callResp := deviceSession.Call(deskconn.ProcedurePrinterPrint).Args(*printerPrintPrinter, filename, data).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		if len(callResp.Args()) > 0 {
			fmt.Printf("print job queued: %v\n", callResp.Args()[0])
		} else {
			fmt.Println("print job queued")
		}

	case portForwardCmd.FullCommand():
		var localPort, remotePort string
		if *portForwardPorts != "" && (*portForwardLocalFlag != "" || *portForwardRemoteFlag != "") {
			fmt.Fprintln(os.Stderr, "specify ports with either localport:remoteport or --local and --remote, not both")
			return
		}

		if *portForwardPorts != "" {
			parts := strings.SplitN(*portForwardPorts, ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				fmt.Fprintln(os.Stderr, "invalid port mapping: use localport:remoteport")
				return
			}
			localPort, remotePort = parts[0], parts[1]
		} else if *portForwardLocalFlag != "" && *portForwardRemoteFlag != "" {
			localPort = *portForwardLocalFlag
			remotePort = *portForwardRemoteFlag
		} else if *portForwardLocalFlag != "" || *portForwardRemoteFlag != "" {
			fmt.Fprintln(os.Stderr, "both --local(-l) and --remote(-r) must be provided together")
			return
		} else {
			fmt.Fprintln(os.Stderr, "specify ports as localport:remoteport or use --local and --remote flags")
			return
		}

		realm, err := deviceRealm(*portForwardDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		deviceSession, err := deskconn.ConnectDeviceRealm(context.Background(), realm, cfgDirectory, *portForwardP2PFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("Forwarding 127.0.0.1:%s -> %s:%s\n", localPort, *portForwardDevice, remotePort)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- deskconn.ForwardLocalPort(ctx, deviceSession, remotePort, localPort)
		}()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sigCh:
			fmt.Println("\nStopping port forwarding...")
			cancel()
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case lsCmd.FullCommand():
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}

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

		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
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

	case selfUpdateCmd.FullCommand():
		if err := updateApp(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}
}

type appUpdateResponse struct {
	DownloadURL   string `json:"download_url"`
	LatestVersion string `json:"latest_version"`
}

func updateApp(cfgDirectory string) error {
	fmt.Printf("Checking for updates for %s on %s-%s...\n", version, runtime.GOOS, runtime.GOARCH)

	cloudSession, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), deskconn.ErrAuthenticationFailed) {
			_ = deskconn.RemoveCredentialsFiles(cfgDirectory)
			return fmt.Errorf("invalid credentials, please login again")
		}
		return err
	}

	callResp := cloudSession.Call(deskconn.ProcedureAppUpdateCheck).Args("deskconn", version, runtime.GOOS,
		runtime.GOARCH).Do()
	if callResp.Err != nil {
		return callResp.Err
	}

	if len(callResp.Args()) == 0 {
		fmt.Printf("You're already on version %s of deskconn (the latest version).\n", version)
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

	fmt.Printf("Found update at %s.\n", updateResp.DownloadURL)

	if err := downloadAndInstallUpdate(updateResp.DownloadURL); err != nil {
		return err
	}

	fmt.Printf("Restarting deskconnd service...\n")
	cmd := exec.Command("systemctl", "--user", "restart", "deskconnd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart deskconnd: %w", err)
	}

	fmt.Printf("Updated deskconn from version %s to %s.\n", version, updateResp.LatestVersion)
	return nil
}

func downloadAndInstallUpdate(downloadURL string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	fmt.Printf("Downloading update from %s...\n", downloadURL)
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
	fmt.Println("Extracting update archive...")

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
			fmt.Println("Installing deskconn...")
			deskconnPath := filepath.Join(binDir, "deskconn")
			if err := installBinaryFromReader(tarReader, deskconnPath, 0755); err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(binDir, "desk"))
			if err := os.Symlink(deskconnPath, filepath.Join(binDir, "desk")); err != nil {
				return fmt.Errorf("failed to create desk symlink: %w", err)
			}
			foundDeskconn = true
		case "deskconnd":
			fmt.Println("Installing deskconnd...")
			if err := installBinaryFromReader(tarReader, filepath.Join(execDir, "deskconnd"), 0700); err != nil {
				return err
			}
			foundDeskconnd = true
		}
	}

	if !foundDeskconn || !foundDeskconnd {
		return fmt.Errorf("update archive missing required binaries")
	}

	deskconnBin := filepath.Join(binDir, "deskconn")
	if err := installCompletions(deskconnBin); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update shell completions: %v\n", err)
	}

	return nil
}

func installCompletions(deskconnBin string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	bashDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	for _, dir := range []string{bashDir, zshDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	bashOut, err := exec.Command(deskconnBin, "--completion-script-bash").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating bash completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(bashDir, "deskconn"), bashOut, 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}
	deskBash := strings.Replace(string(bashOut),
		"complete -F _deskconn_bash_autocomplete -o default deskconn",
		"complete -F _deskconn_bash_autocomplete -o default desk", 1)
	if err := os.WriteFile(filepath.Join(bashDir, "desk"), []byte(deskBash), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing desk bash completion: %w", err)
	}

	zshOut, err := exec.Command(deskconnBin, "--completion-script-zsh").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating zsh completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(zshDir, "_deskconn"), zshOut, 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing zsh completion: %w", err)
	}
	deskZsh := "#compdef desk\n_deskconn \"$@\"\n"
	if err := os.WriteFile(filepath.Join(zshDir, "_desk"), []byte(deskZsh), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing desk zsh completion: %w", err)
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

func attach(flagUsername, flagPassword, name string, useStdin bool) error {
	file, err := deskconn.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("device already attached")
	}

	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
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

func detach(flagUsername, flagPassword string, useStdin bool) error {
	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
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

func login(flagUsername, flagPassword string, useStdin bool) error {
	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
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
		if strings.Contains(err.Error(), deskconn.ErrAuthenticationFailed) {
			return deskconn.RemoveCredentialsFiles(cfgDirectory)
		}
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

	return deskconn.RemoveCredentialsFiles(cfgDirectory)
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

func readCredentials(flagUsername, flagPassword string, useStdin bool) (username, password string, err error) {
	if useStdin {
		if flagUsername == "" {
			return "", "", fmt.Errorf("--username is required when using --password-stdin")
		}
		if flagPassword == "" {
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return "", "", fmt.Errorf("reading password from stdin: %w", err)
			}
			flagPassword = strings.TrimRight(line, "\r\n")
		}
		return flagUsername, flagPassword, nil
	}

	if flagUsername == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
			return "", "", fmt.Errorf("username required: use --username flag")
		}
		fmt.Fprint(os.Stderr, "Username: ")
		reader := bufio.NewReader(os.Stdin)
		pwd, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		flagUsername = strings.TrimSpace(pwd)
		if flagUsername == "" {
			return "", "", fmt.Errorf("username cannot be empty")
		}
	}

	if flagPassword == "" {
		flagPassword, err = readPassword()
		if err != nil {
			return "", "", err
		}
	}

	return flagUsername, flagPassword, nil
}

func readPassword() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("password required: use --password or --password-stdin flag")
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
