package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/terminal"
	"github.com/xconnio/deskconn/internal/userd"
)

var version = "v0.1.0-beta"

const (
	ModeRouted = deskconn.ModeRouted
	ModeDirect = deskconn.ModeDirect

	jsonFieldPath = "path"

	// goosWindows is runtime.GOOS's value on Windows.
	goosWindows = "windows"
)

func main() {
	// Checked once the command is known: the root helper (desk serve --mode root) doesn't
	// need it, and runs under systemd-run, possibly without a HOME.
	cfgDirectory, cfgErr := common.CfgDirectory()
	deskconn.SetCloud(accountServiceCloud())

	app := kingpin.New("desk", "Control your machines remotely.")
	configureHelp(app)
	keygenCmd := app.Command("keygen", "Generate a key pair for logging in to standalone machines")
	machineCmds := registerMachineCommands(app, cfgDirectory)
	authCmds := registerAuthCommands(app)
	fileCmds := registerFileCommands(app, cfgDirectory)
	shellCmds := registerShellCommands(app, cfgDirectory)
	portCmds := registerPortCommands(app, cfgDirectory)
	vpnCmds := registerVPNCommands(app, cfgDirectory)
	machineCmds.registerLsShortcut(app)
	appCmds := registerAppCommands(app, cfgDirectory)
	selfCmds := registerSelfCommands(app)
	agentCmds := registerAgentCommands(app, cfgDirectory)
	serveCmds := registerServeCommands(app)
	topCmds := registerTopCommands(app)
	terminalWorkerCmd := app.Command(userd.TerminalWorkerCommand,
		"Run a persistent terminal session; the daemon starts these").Hidden()

	if len(os.Args) == 1 {
		app.Usage(nil)
		return
	}
	// A command given without the subcommand or machine it needs gets its usage, as
	// an invalid command line.
	if len(os.Args) == 2 {
		if os.Args[1] == "shell" {
			app.Usage(os.Args[1:])
			os.Exit(exitUsage)
		}
		for _, command := range app.Model().Commands {
			if command.Name == os.Args[1] && len(command.Commands) > 0 && !hasDefaultCommand(command) {
				app.Usage(os.Args[1:])
				os.Exit(exitUsage)
			}
		}
	}

	parsedCmd, parseErr := app.Parse(os.Args[1:])
	if parseErr != nil {
		if errors.Is(parseErr, kingpin.ErrCommandNotSpecified) {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %s\n\n", parseErr)
		ctx, _ := app.ParseContext(os.Args[1:])
		if ctx != nil {
			_ = app.UsageForContext(ctx)
		} else {
			app.Usage(os.Args[1:])
		}
		os.Exit(exitUsage)
	}

	// A terminal worker is handed everything it needs by the daemon that starts it.
	if parsedCmd == terminalWorkerCmd.FullCommand() {
		if err := terminal.RunWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}

	if cfgErr != nil && parsedCmd != serveCmds.run.FullCommand() {
		log.Fatal(cfgErr)
	}

	if parsedCmd == keygenCmd.FullCommand() {
		exitOnErr(printKeyPair())
		return
	}

	_ = dispatchMachineCommand(parsedCmd, machineCmds, cfgDirectory) ||
		dispatchAuthCommand(parsedCmd, authCmds, cfgDirectory) ||
		dispatchFileCommand(parsedCmd, fileCmds, cfgDirectory) ||
		dispatchShellCommand(parsedCmd, shellCmds, cfgDirectory) ||
		dispatchPortCommand(parsedCmd, portCmds, cfgDirectory) ||
		dispatchVPNCommand(parsedCmd, vpnCmds, cfgDirectory) ||
		dispatchAppCommand(parsedCmd, appCmds, cfgDirectory) ||
		dispatchSelfCommand(parsedCmd, selfCmds, cfgDirectory) ||
		dispatchAgentCommand(parsedCmd, agentCmds, cfgDirectory) ||
		dispatchServeCommand(parsedCmd, serveCmds) ||
		dispatchTopCommand(parsedCmd, topCmds, cfgDirectory)
}

// hasDefaultCommand reports whether command runs one of its subcommands when given none
// (desk serve), rather than needing one named.
func hasDefaultCommand(command *kingpin.CmdModel) bool {
	for _, sub := range command.Commands {
		if sub.Default {
			return true
		}
	}
	return false
}

// runUntilInterrupted runs run until it returns, or until Ctrl+C/SIGTERM, which prints
// stopMessage and cancels run's context.
func runUntilInterrupted(stopMessage string, run func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	common.SafeGo(func() {
		errCh <- run(ctx)
	})

	select {
	case <-sigCh:
		fmt.Println("\n" + stopMessage)
		cancel()
		return nil
	case err := <-errCh:
		return err
	}
}

// machineArg defines cmd's machine argument: a managed or standalone machine.
func machineArg(cmd *kingpin.CmdClause, help, cfgDirectory string) *string {
	return cmd.Arg("machine", help).Required().HintAction(machineCompletions(cfgDirectory)).String()
}

// modeFlag defines cmd's --mode flag. Left out, a managed machine is reached through the
// local daemon and a standalone one, which the daemon doesn't know, routed (see defaultMode).
func modeFlag(cmd *kingpin.CmdClause, help string) *string {
	return cmd.Flag("mode", help).Enum(ModeRouted, ModeDirect)
}
