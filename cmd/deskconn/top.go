package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/cmd/deskconn/top"
)

type topCommands struct {
	top *kingpin.CmdClause
}

func registerTopCommands(app *kingpin.Application) *topCommands {
	return &topCommands{
		top: app.Command("top", "Interactive view of your machines and profiles"),
	}
}

func dispatchTopCommand(parsedCmd string, cmds *topCommands, cfgDirectory string) bool {
	if parsedCmd != cmds.top.FullCommand() {
		return false
	}
	err := errors.New("desk top lists every profile; run it without --profile")
	if !standaloneRequested() {
		err = top.Run(cfgDirectory, version)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return true
}
