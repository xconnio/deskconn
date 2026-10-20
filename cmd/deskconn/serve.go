package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
)

// What "desk serve" runs (--mode).
const (
	serveModeUser       = "user"
	serveModeStandalone = "standalone"
	serveModeRoot       = "root"
)

// Standalone transports (--transport).
const (
	transportQUIC         = "quic"
	transportWebTransport = "webtransport"
	transportBoth         = "both"
)

// userdFlags configure the user daemon (desk serve --mode user or standalone).
type userdFlags struct {
	standaloneTransport *string
	standaloneQUICAddr  *string
	standaloneWTAddr    *string
	standaloneCert      *string
	standaloneKey       *string
	standaloneKeys      *[]string
	standaloneUsers     *[]string
}

// rootdFlags configure the root helper (desk serve --mode root).
type rootdFlags struct {
	socketPath  *string
	idleTimeout *time.Duration
}

type serveCommands struct {
	serve *kingpin.CmdClause

	mode  *string
	userd userdFlags
	rootd rootdFlags
}

func registerServeCommands(app *kingpin.Application) *serveCommands {
	serveCmd := app.Command("serve", "Run the deskconn daemon: the user daemon, or the root helper for desk vpn")
	mode := serveCmd.Flag("mode", "What to run: user (the machine's APIs and its connectivity, through the "+
		"cloud), standalone (the same, served directly over --transport to --public-key holders and --user "+
		"accounts instead of through the cloud) or root (the privileged backend for desk vpn, started on "+
		"demand via sudo)").
		Default(serveModeUser).Enum(serveModeUser, serveModeStandalone, serveModeRoot)

	userd := userdFlags{
		standaloneTransport: serveCmd.Flag("transport", "Standalone transport to serve: quic, webtransport or both").
			Default(transportQUIC).Enum(transportQUIC, transportWebTransport, transportBoth),
		standaloneQUICAddr: serveCmd.Flag("quic-address", "Where to serve QUIC in standalone mode (host:port, UDP)").
			Default("0.0.0.0:18080").String(),
		standaloneWTAddr: serveCmd.Flag("webtransport-address", "Where to serve WebTransport in standalone mode "+
			"(host:port, UDP)").Default("0.0.0.0:18081").String(),
		standaloneCert: serveCmd.Flag("tls-cert", "TLS certificate (PEM) for standalone mode; without it, a "+
			"self-signed one is generated and kept in the config directory").String(),
		standaloneKey: serveCmd.Flag("tls-key", "Private key (PEM) of --tls-cert").String(),
		standaloneKeys: serveCmd.Flag("public-key", "Public key (hex) allowed to connect in standalone mode; "+
			"repeat for more (see `desk keygen`)").Strings(),
		standaloneUsers: serveCmd.Flag("user", "username:password allowed to connect in standalone mode; repeat "+
			"for more").Envar("DESKCONND_USERS").Strings(),
	}

	rootd := rootdFlags{
		socketPath: serveCmd.Flag("socket", "Unix socket to listen on for the one client the root helper "+
			"serves (--mode root)").String(),
		idleTimeout: serveCmd.Flag("idle-timeout", "Exit the root helper if no client connects within this long "+
			"(--mode root)").Default("30s").Duration(),
	}

	return &serveCommands{
		serve: serveCmd,
		mode:  mode,
		userd: userd,
		rootd: rootd,
	}
}

func dispatchServeCommand(parsedCmd string, cmds *serveCommands) bool {
	if parsedCmd != cmds.serve.FullCommand() {
		return false
	}
	switch *cmds.mode {
	case serveModeRoot:
		if *cmds.rootd.socketPath == "" {
			serveFatalf("--mode root needs --socket")
		}
		runRootd(cmds.rootd)
	default:
		runUserd(cmds.userd, *cmds.mode == serveModeStandalone)
	}
	return true
}

// serveFatalf reports invalid desk serve usage and exits.
func serveFatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "desk serve: "+format+"\n", args...)
	os.Exit(1)
}
