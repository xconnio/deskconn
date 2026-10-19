package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
)

var version = "v0.1.0-alpha"

const (
	ModeRouted = "routed"
	ModeDirect = "direct"

	jsonFieldPath = "path"
)

func main() {
	// One binary, several programs: desk (or deskconn) is the CLI, and the daemons run
	// from symlinks named after them.
	if args, ok := multicallArgs(os.Args, userdName); ok {
		runUserd(args)
		return
	}
	if args, ok := multicallArgs(os.Args, deskconn.RootdName); ok {
		runRootd(args)
		return
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	app := kingpin.New("desk", "Control your machines remotely.")
	configureHelp(app)
	profileName := app.Flag("profile", "Connect directly to the standalone machine saved as this profile "+
		"(see `desk profile add`) instead of a machine on your account").
		HintAction(profileCompletions(cfgDirectory)).String()

	profileCmds := registerProfileCommands(app, cfgDirectory)
	machineCmds := registerMachineCommands(app, cfgDirectory)
	accountCmds := registerAccountCommands(app)
	fileCmds := registerFileCommands(app, cfgDirectory)
	shellCmds := registerShellCommands(app, cfgDirectory)
	portCmds := registerPortCommands(app, cfgDirectory)
	vpnCmds := registerVPNCommands(app, cfgDirectory)
	machineCmds.registerLsShortcut(app)
	selfCmds := registerSelfCommands(app)
	agentCmds := registerAgentCommands(app, cfgDirectory)

	if len(os.Args) == 1 {
		app.Usage(nil)
		return
	}
	if len(os.Args) == 2 {
		if os.Args[1] == "shell" && !standaloneRequested() {
			app.Usage(os.Args[1:])
			return
		}
		for _, command := range app.Model().Commands {
			if command.Name == os.Args[1] && len(command.Commands) > 0 {
				app.Usage(os.Args[1:])
				return
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
		os.Exit(1)
	}

	if *profileName != "" {
		if err := useProfile(cfgDirectory, *profileName); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if machineCmds.accountOnly(parsedCmd) {
			fmt.Fprintf(os.Stderr, "%s is not available with --profile (it works on machines of your account)\n",
				parsedCmd)
			os.Exit(1)
		}
	}

	_ = dispatchProfileCommand(parsedCmd, profileCmds, cfgDirectory) ||
		dispatchMachineCommand(parsedCmd, machineCmds, cfgDirectory) ||
		dispatchAccountCommand(parsedCmd, accountCmds, cfgDirectory) ||
		dispatchFileCommand(parsedCmd, fileCmds, cfgDirectory) ||
		dispatchShellCommand(parsedCmd, shellCmds, cfgDirectory) ||
		dispatchPortCommand(parsedCmd, portCmds, cfgDirectory) ||
		dispatchVPNCommand(parsedCmd, vpnCmds, cfgDirectory) ||
		dispatchSelfCommand(parsedCmd, selfCmds, cfgDirectory) ||
		dispatchAgentCommand(parsedCmd, agentCmds, cfgDirectory)
}

// daemonURI is the address of deskconn.sock, the local daemon's socket.
func daemonURI(cfgDirectory string) string {
	return common.UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// printErr reports a failed command on stderr. Most commands still exit 0 after.
func printErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
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

// standaloneRequested reports whether the CLI is pointed at a standalone machine
// (--profile). It has to be known before the command line is parsed: machine arguments
// aren't defined then.
func standaloneRequested() bool {
	for _, arg := range os.Args[1:] {
		if arg == "--" {
			break
		}
		if arg == "--profile" || strings.HasPrefix(arg, "--profile=") {
			return true
		}
	}
	return false
}

// machineArg defines cmd's machine argument. With --profile there is only one machine, so
// the argument isn't defined at all and the result is empty.
func machineArg(cmd *kingpin.CmdClause, help, cfgDirectory string) *string {
	if standaloneRequested() {
		return new(string)
	}
	return cmd.Arg("machine", help).Required().HintAction(machineCompletions(cfgDirectory)).String()
}

// modeFlag defines cmd's --mode flag. With --profile it defaults to a direct connection: the
// local daemon that some commands go through by default doesn't know the standalone machine.
func modeFlag(cmd *kingpin.CmdClause, help string) *string {
	flag := cmd.Flag("mode", help)
	if standaloneRequested() {
		flag = flag.Default(ModeRouted)
	}
	return flag.Enum(ModeRouted, ModeDirect)
}
