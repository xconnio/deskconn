package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"

	"github.com/xconnio/deskconn/deskconn"
)

type portCommands struct {
	forward *kingpin.CmdClause
	reverse *kingpin.CmdClause

	forwardMachine *string
	forwardPorts   *string
	forwardMode    *string

	reverseMachine *string
	reversePorts   *string
	reverseMode    *string
}

func registerPortCommands(app *kingpin.Application, cfgDirectory string) *portCommands {
	const modeHelp = "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC"

	portCmd := app.Command("port", "Forward ports")

	forwardCmd := portCmd.Command("forward", "Listen locally and forward connections to a port on the remote machine")
	forwardMachine := machineArg(forwardCmd, "ID, name or alias of machine", cfgDirectory)
	forwardPorts := forwardCmd.Arg("listen:target",
		"Listen on local 127.0.0.1:LISTEN and connect to remote localhost:TARGET (e.g. 8080:80)").Required().String()
	forwardMode := modeFlag(forwardCmd, modeHelp)

	reverseCmd := portCmd.Command("reverse", "Listen on the remote machine and forward connections to a local port")
	reverseMachine := machineArg(reverseCmd, "ID, name or alias of machine", cfgDirectory)
	reversePorts := reverseCmd.Arg("listen:target",
		"Listen on remote port LISTEN (all interfaces) and connect to local localhost:TARGET "+
			"(e.g. 9000:3000)").Required().String()
	reverseMode := modeFlag(reverseCmd, modeHelp)

	return &portCommands{
		forward:        forwardCmd,
		reverse:        reverseCmd,
		forwardMachine: forwardMachine,
		forwardPorts:   forwardPorts,
		forwardMode:    forwardMode,
		reverseMachine: reverseMachine,
		reversePorts:   reversePorts,
		reverseMode:    reverseMode,
	}
}

func dispatchPortCommand(parsedCmd string, cmds *portCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.forward.FullCommand():
		printErr(forwardPort(cfgDirectory, *cmds.forwardMachine, *cmds.forwardPorts, *cmds.forwardMode))
	case cmds.reverse.FullCommand():
		printErr(reversePort(cfgDirectory, *cmds.reverseMachine, *cmds.reversePorts, *cmds.reverseMode))
	default:
		return false
	}
	return true
}

func forwardPort(cfgDirectory, machine, mapping, mode string) error {
	localPort, remotePort, err := parsePortMapping(mapping)
	if err != nil {
		return err
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	fmt.Printf("Forwarding 127.0.0.1:%s -> %s:%s\n", localPort, machine, remotePort)
	return runUntilInterrupted("Stopping port forwarding...", func(ctx context.Context) error {
		return deskconn.RunPortForward(ctx, mode, realm, cfgDirectory, remotePort, localPort)
	})
}

func reversePort(cfgDirectory, machine, mapping, mode string) error {
	remotePort, localPort, err := parsePortMapping(mapping)
	if err != nil {
		return err
	}
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	fmt.Printf("Reverse forwarding %s:%s -> 127.0.0.1:%s\n", machine, remotePort, localPort)
	return runUntilInterrupted("Stopping reverse port forwarding...", func(ctx context.Context) error {
		return deskconn.RunPortReverse(ctx, mode, realm, cfgDirectory, remotePort, localPort)
	})
}

func parsePortMapping(mapping string) (listen, target string, err error) {
	listen, target, ok := strings.Cut(mapping, ":")
	if !ok || listen == "" || target == "" {
		return "", "", fmt.Errorf("invalid port mapping: use listen:target (e.g. 8080:80)")
	}
	for _, port := range []string{listen, target} {
		n, parseErr := strconv.ParseUint(port, 10, 16)
		if parseErr != nil || n == 0 {
			return "", "", fmt.Errorf("invalid port %q: use a number from 1 to 65535", port)
		}
	}
	return listen, target, nil
}
