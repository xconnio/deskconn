package main

import (
	"context"
	"runtime"

	"github.com/alecthomas/kingpin/v2"
)

type vpnCommands struct {
	connect *kingpin.CmdClause
	serve   *kingpin.CmdClause
	stop    *kingpin.CmdClause

	connectMachine *string
}

func registerVPNCommands(app *kingpin.Application, cfgDirectory string) *vpnCommands {
	vpnCmd := app.Command("vpn", "Route traffic through a machine or serve as an exit node")
	if runtime.GOOS != "linux" {
		vpnCmd.Hidden()
	}
	connectCmd := vpnCmd.Command("connect", "Connect to a machine and tunnel all traffic through it")
	connectMachine := machineArg(connectCmd, "ID, name or alias of machine", cfgDirectory)
	serveCmd := vpnCmd.Command("serve", "Let other machines route their traffic through this machine")
	stopCmd := vpnCmd.Command("stop", "Stop serving as a VPN exit node")

	return &vpnCommands{
		connect:        connectCmd,
		serve:          serveCmd,
		stop:           stopCmd,
		connectMachine: connectMachine,
	}
}

func dispatchVPNCommand(parsedCmd string, cmds *vpnCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.connect.FullCommand():
		realm, err := machineRealm(*cmds.connectMachine, cfgDirectory)
		if err != nil {
			printErr(err)
			return true
		}
		ctx, cancel := context.WithCancel(context.Background())
		runVPNConnect(ctx, cfgDirectory, realm, *cmds.connectMachine)
		cancel()
	case cmds.serve.FullCommand():
		ctx, cancel := context.WithCancel(context.Background())
		runVPNServe(ctx, cfgDirectory)
		cancel()
	case cmds.stop.FullCommand():
		runVPNStop(context.Background(), cfgDirectory)
	default:
		return false
	}
	return true
}
