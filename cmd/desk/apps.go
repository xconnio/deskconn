package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/xconn-go"
)

type appsCommands struct {
	ls      *kingpin.CmdClause
	enable  *kingpin.CmdClause
	disable *kingpin.CmdClause

	lsMachine *string
	lsMode    *string

	enableApp     *string
	enableMachine *string
	enableMode    *string

	disableApp     *string
	disableMachine *string
	disableMode    *string
}

type appState struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Requires []string `json:"requires"`
}

func registerAppsCommands(app *kingpin.Application, cfgDirectory string) *appsCommands {
	appsCmd := app.Command("apps", "Turn a device's apps on or off (changing them needs owner or admin access)")
	const modeHelp = "Connection mode: 'quic' uses QUIC stream via router, default uses daemon persistent session"

	// machine is optional: omitting it targets this device.
	machineArg := func(cmd *kingpin.CmdClause) *string {
		if standaloneRequested() {
			return new(string)
		}
		return cmd.Arg("machine", "Device (default: this device)").HintAction(deviceCompletions(cfgDirectory)).String()
	}

	lsCmd := appsCmd.Command("ls", "List apps and whether they're enabled")
	lsMachine := machineArg(lsCmd)
	lsMode := modeFlag(lsCmd, modeHelp)

	enableCmd := appsCmd.Command("enable", "Enable an app")
	enableApp := enableCmd.Arg("app", "App id (see `apps ls`)").Required().String()
	enableMachine := machineArg(enableCmd)
	enableMode := modeFlag(enableCmd, modeHelp)

	disableCmd := appsCmd.Command("disable", "Disable an app, and every app that requires it")
	disableApp := disableCmd.Arg("app", "App id (see `apps ls`)").Required().String()
	disableMachine := machineArg(disableCmd)
	disableMode := modeFlag(disableCmd, modeHelp)

	return &appsCommands{
		ls: lsCmd, enable: enableCmd, disable: disableCmd,
		lsMachine: lsMachine, lsMode: lsMode,
		enableApp: enableApp, enableMachine: enableMachine, enableMode: enableMode,
		disableApp: disableApp, disableMachine: disableMachine, disableMode: disableMode,
	}
}

func dispatchAppsCommand(parsedCmd string, cmds *appsCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand():
		apps, _, err := callCapabilities(cfgDirectory, *cmds.lsMachine, *cmds.lsMode)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.Header([]string{"APP", "NAME", "ENABLED", "REQUIRES"})
		for _, a := range apps {
			_ = table.Append([]any{a.ID, a.Name, a.Enabled, strings.Join(a.Requires, ", ")})
		}
		if err := table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	case cmds.enable.FullCommand():
		_, changed, err := callCapabilities(cfgDirectory, *cmds.enableMachine, *cmds.enableMode, *cmds.enableApp, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if !changed {
			fmt.Printf("%s is already enabled on %s\n", *cmds.enableApp, machineLabel(*cmds.enableMachine))
			return true
		}
		fmt.Printf("enabled %s on %s\n", *cmds.enableApp, machineLabel(*cmds.enableMachine))
	case cmds.disable.FullCommand():
		apps, changed, err := callCapabilities(cfgDirectory, *cmds.disableMachine, *cmds.disableMode,
			*cmds.disableApp, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if !changed {
			fmt.Printf("%s is already disabled on %s\n", *cmds.disableApp, machineLabel(*cmds.disableMachine))
			return true
		}
		fmt.Printf("disabled %s on %s\n", *cmds.disableApp, machineLabel(*cmds.disableMachine))
		var dependents []string
		for _, a := range apps {
			if slices.Contains(a.Requires, *cmds.disableApp) {
				dependents = append(dependents, a.ID)
			}
		}
		if len(dependents) > 0 {
			fmt.Printf("also disabled %s, which require %s\n", strings.Join(dependents, ", "), *cmds.disableApp)
		}
	default:
		return false
	}
	return true
}

// callCapabilities lists machine's apps, or with setArgs (app id, enabled) changes one
// first, and returns the resulting list and whether the change did anything. An empty machine is this device.
func callCapabilities(cfgDirectory, machine, mode string, setArgs ...any) (apps []appState, changed bool, err error) {
	procedure, proxyProcedure := common.ProcedureCapabilitiesList, common.ProcedureProxyCapabilitiesList
	if len(setArgs) > 0 {
		procedure, proxyProcedure = common.ProcedureCapabilitiesSet, common.ProcedureProxyCapabilitiesSet
	}

	var resp xconn.CallResponse
	if machine == "" && !standaloneRequested() {
		// This device: its own deskconnd serves the procedures on the local realm.
		localSession, err := xconn.ConnectAnonymous(context.Background(),
			fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory), common.LocalRealm)
		if err != nil {
			return nil, false, fmt.Errorf("could not reach local daemon: %w", err)
		}
		defer func() { _ = localSession.Leave() }()
		resp = localSession.Call(procedure).Args(setArgs...).Do()
	} else {
		realm, err := deviceRealm(machine, cfgDirectory)
		if err != nil {
			return nil, false, fmt.Errorf("unknown device %q: %w", machine, err)
		}
		switch mode {
		case ModeQUIC:
			quicSess, err := common.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				return nil, false, err
			}
			defer quicSess.Connection().Close()
			resp = quicSess.Session.Call(procedure).Args(setArgs...).Do()
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				return nil, false, err
			}
			defer func() { _ = p2pSess.Leave() }()
			resp = p2pSess.Call(procedure).Args(setArgs...).Do()
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(),
				fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory), common.LocalRealm)
			if err != nil {
				return nil, false, fmt.Errorf("could not reach local daemon: %w", err)
			}
			defer func() { _ = localSession.Leave() }()
			resp = localSession.Call(proxyProcedure).Args(append([]any{realm}, setArgs...)...).Do()
		}
	}
	if resp.Err != nil {
		action := "list apps"
		if len(setArgs) > 0 {
			action = fmt.Sprintf("enable app %q", setArgs[0])
			if setArgs[1] == false {
				action = fmt.Sprintf("disable app %q", setArgs[0])
			}
		}
		return nil, false, fmt.Errorf("failed to %s on %s: %s", action, machineLabel(machine), wampErrorMessage(resp.Err))
	}
	if len(resp.Args()) == 0 {
		return nil, false, fmt.Errorf("unexpected empty response")
	}

	data, err := json.Marshal(resp.Args()[0])
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(data, &apps); err != nil {
		return nil, false, fmt.Errorf("expected a list of apps: %w", err)
	}
	changed, _ = resp.ArgBool(1)
	return apps, changed, nil
}

func machineLabel(machine string) string {
	switch {
	case machine != "":
		return machine
	case standaloneRequested():
		return "the standalone device"
	default:
		return "this device"
	}
}

// wampErrorMessage returns err's message without the WAMP error URIs in front of it. The
// local daemon and xlink each wrap a device's error in their own, e.g.
// "wamp.error.operation_failed: wamp.error.invalid_argument: app not found: x".
func wampErrorMessage(err error) string {
	msg := err.Error()
	for strings.HasPrefix(msg, "wamp.error.") {
		_, rest, ok := strings.Cut(msg, ": ")
		if !ok {
			break
		}
		msg = rest
	}
	return msg
}
