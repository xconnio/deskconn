package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/internal/common"
)

type appCommands struct {
	ls         *kingpin.CmdClause
	register   *kingpin.CmdClause
	unregister *kingpin.CmdClause
	enable     *kingpin.CmdClause
	disable    *kingpin.CmdClause

	lsMachine         *string
	registerMachine   *string
	registerDirectory *string
	unregisterMachine *string
	unregisterID      *string
	enableMachine     *string
	enableID          *string
	disableMachine    *string
	disableID         *string
}

func registerAppCommands(app *kingpin.Application, cfgDirectory string) *appCommands {
	const machineHelp = "ID, name or alias of machine"

	appCmd := app.Command("app", "Manage a machine's apps")
	cmds := &appCommands{}

	cmds.ls = appCmd.Command("ls", "List a machine's apps")
	cmds.lsMachine = machineArg(cmds.ls, machineHelp, cfgDirectory)

	cmds.register = appCmd.Command("register", "Register a manifest app, or replace the one with the same id")
	cmds.registerMachine = machineArg(cmds.register, machineHelp, cfgDirectory)
	cmds.registerDirectory = cmds.register.Arg("dir", "Directory with the app's app.yaml").Required().ExistingDir()

	cmds.unregister = appCmd.Command("unregister", "Remove a manifest app")
	cmds.unregisterMachine = machineArg(cmds.unregister, machineHelp, cfgDirectory)
	cmds.unregisterID = cmds.unregister.Arg("id", "App id").Required().String()

	cmds.enable = appCmd.Command("enable", "Enable an app")
	cmds.enableMachine = machineArg(cmds.enable, machineHelp, cfgDirectory)
	cmds.enableID = cmds.enable.Arg("id", "App id").Required().String()

	cmds.disable = appCmd.Command("disable", "Disable an app")
	cmds.disableMachine = machineArg(cmds.disable, machineHelp, cfgDirectory)
	cmds.disableID = cmds.disable.Arg("id", "App id").Required().String()

	return cmds
}

func dispatchAppCommand(parsedCmd string, cmds *appCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.ls.FullCommand():
		printErr(listApps(cfgDirectory, *cmds.lsMachine))
	case cmds.register.FullCommand():
		printErr(registerApp(cfgDirectory, *cmds.registerMachine, *cmds.registerDirectory))
	case cmds.unregister.FullCommand():
		printErr(unregisterApp(cfgDirectory, *cmds.unregisterMachine, *cmds.unregisterID))
	case cmds.enable.FullCommand():
		printErr(setAppEnabled(cfgDirectory, *cmds.enableMachine, *cmds.enableID, true))
	case cmds.disable.FullCommand():
		printErr(setAppEnabled(cfgDirectory, *cmds.disableMachine, *cmds.disableID, false))
	default:
		return false
	}
	return true
}

// callMachine calls procedure with args on machine, routed over QUIC, and returns its
// first result.
func callMachine(cfgDirectory, machine, procedure string, args ...any) (any, error) {
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return nil, err
	}
	session, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	resp := session.Call(procedure).Args(args...).Do()
	if resp.Err != nil {
		return nil, resp.Err
	}
	if resp.ArgsLen() == 0 {
		return nil, nil
	}
	return resp.Args()[0], nil
}

func listApps(cfgDirectory, machine string) error {
	result, err := callMachine(cfgDirectory, machine, common.ProcedureCapabilitiesList)
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var apps []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Enabled  bool     `json:"enabled"`
		Requires []string `json:"requires"`
		Manifest bool     `json:"manifest"`
	}
	if err := json.Unmarshal(data, &apps); err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tENABLED\tMANIFEST\tREQUIRES")
	for _, a := range apps {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%t\t%t\t%s\n", a.ID, a.Name, a.Enabled, a.Manifest,
			strings.Join(a.Requires, ","))
	}
	return w.Flush()
}

func registerApp(cfgDirectory, machine, directory string) error {
	files, err := common.ReadManifestFiles(directory)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(files)
	if err != nil {
		return err
	}

	id, err := callMachine(cfgDirectory, machine, common.ProcedureManifestRegister, payload)
	if err != nil {
		return err
	}
	fmt.Printf("registered %v on %s\n", id, machine)
	return nil
}

func unregisterApp(cfgDirectory, machine, id string) error {
	if _, err := callMachine(cfgDirectory, machine, common.ProcedureManifestUnregister, id); err != nil {
		return err
	}
	fmt.Printf("unregistered %s from %s\n", id, machine)
	return nil
}

func setAppEnabled(cfgDirectory, machine, id string, enabled bool) error {
	if _, err := callMachine(cfgDirectory, machine, common.ProcedureCapabilitiesSet, id, enabled); err != nil {
		return err
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	fmt.Printf("%s %s on %s\n", state, id, machine)
	return nil
}
