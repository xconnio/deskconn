package main

import (
	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/cmd/deskconn/top"
)

type topCommands struct {
	top *kingpin.CmdClause
}

func registerTopCommands(app *kingpin.Application) *topCommands {
	return &topCommands{
		top: app.Command("top", "Interactive view of your managed and standalone machines"),
	}
}

func dispatchTopCommand(parsedCmd string, cmds *topCommands, cfgDirectory string) bool {
	if parsedCmd != cmds.top.FullCommand() {
		return false
	}
	exitOnErr(top.Run(cfgDirectory, version))
	return true
}
