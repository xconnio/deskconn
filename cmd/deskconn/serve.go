package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"
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
	run  *kingpin.CmdClause
	init *kingpin.CmdClause

	initPath  *string
	initForce *bool

	mode       *string
	configPath *string
	userd      userdFlags
	rootd      rootdFlags

	// setByUser tells, by flag name, whether a flag a --config file can also set was given
	// on the command line, which then wins over the file.
	setByUser map[string]*bool
}

func registerServeCommands(app *kingpin.Application) *serveCommands {
	serveCmd := app.Command("serve", "Run the deskconn daemon: the user daemon, or the root helper for desk vpn")
	// "desk serve [<flags>]" runs the daemon: run is the default, so it never has to be named.
	runCmd := serveCmd.Command("run", "Run the daemon; the default, so \"desk serve [<flags>]\" is the same").Default()
	initCmd := serveCmd.Command("init", "Write a sample config file for desk serve --config")
	initPath := initCmd.Arg("path", "Where to write it").Default("serve.yaml").String()
	initForce := initCmd.Flag("force", "Overwrite the file if it exists").Short('f').Bool()

	setByUser := map[string]*bool{}
	flag := func(name, help string) *kingpin.FlagClause {
		set := new(bool)
		setByUser[name] = set
		return runCmd.Flag(name, help).IsSetByUser(set)
	}

	mode := flag("mode", "What to run: user (the machine's APIs and its connectivity, through the "+
		"cloud), standalone (the same, served directly over --transport to --public-key holders and --user "+
		"accounts instead of through the cloud) or root (the privileged backend for desk vpn, started on "+
		"demand via sudo)").
		Default(serveModeUser).Enum(serveModeUser, serveModeStandalone, serveModeRoot)

	userd := userdFlags{
		standaloneTransport: flag("transport", "Standalone transport to serve: quic, webtransport or both").
			Default(transportQUIC).Enum(transportQUIC, transportWebTransport, transportBoth),
		standaloneQUICAddr: flag("quic-address", "Where to serve QUIC in standalone mode (host:port, UDP)").
			Default("0.0.0.0:18080").String(),
		standaloneWTAddr: flag("webtransport-address", "Where to serve WebTransport in standalone mode "+
			"(host:port, UDP)").Default("0.0.0.0:18081").String(),
		standaloneCert: flag("tls-cert", "TLS certificate (PEM) for standalone mode; without it, a "+
			"self-signed one is generated and kept in the config directory").String(),
		standaloneKey: flag("tls-key", "Private key (PEM) of --tls-cert").String(),
		standaloneKeys: runCmd.Flag("public-key", "Public key (hex) allowed to connect in standalone mode; "+
			"repeat for more (see `desk keygen`)").Strings(),
		standaloneUsers: runCmd.Flag("user", "username:password allowed to connect in standalone mode; repeat "+
			"for more").Envar("DESKCONND_USERS").Strings(),
	}
	configPath := runCmd.Flag("config", "YAML file to read the mode and the standalone settings from (see "+
		"the README); flags given as well override it, and --public-key/--user add to its keys and users").
		String()

	rootd := rootdFlags{
		socketPath: runCmd.Flag("socket", "Unix socket to listen on for the one client the root helper "+
			"serves (--mode root)").String(),
		idleTimeout: runCmd.Flag("idle-timeout", "Exit the root helper if no client connects within this long "+
			"(--mode root)").Default("30s").Duration(),
	}

	return &serveCommands{
		run:        runCmd,
		init:       initCmd,
		initPath:   initPath,
		initForce:  initForce,
		mode:       mode,
		configPath: configPath,
		userd:      userd,
		rootd:      rootd,
		setByUser:  setByUser,
	}
}

func dispatchServeCommand(parsedCmd string, cmds *serveCommands) bool {
	if parsedCmd == cmds.init.FullCommand() {
		if err := writeSampleServeConfig(*cmds.initPath, *cmds.initForce); err != nil {
			serveFatalf("%v", err)
		}
		return true
	}
	if parsedCmd != cmds.run.FullCommand() {
		return false
	}
	if *cmds.configPath != "" {
		// A config can't set mode root, so only the flag can have.
		if *cmds.mode == serveModeRoot {
			serveFatalf("--config is for the user and standalone modes")
		}
		config, err := loadServeConfig(*cmds.configPath)
		if err != nil {
			serveFatalf("%v", err)
		}
		cmds.apply(config)
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

// apply sets the flags not given on the command line from config, and adds its users to
// those given.
func (cmds *serveCommands) apply(config *serveConfig) {
	set := func(name string, target *string, value string) {
		if value != "" && !*cmds.setByUser[name] {
			*target = value
		}
	}
	set("mode", cmds.mode, config.Mode)
	set("transport", cmds.userd.standaloneTransport, config.Listen.transport())
	set("quic-address", cmds.userd.standaloneQUICAddr, config.Listen.QUIC)
	set("webtransport-address", cmds.userd.standaloneWTAddr, config.Listen.WebTransport)
	set("tls-cert", cmds.userd.standaloneCert, config.Listen.TLSCert)
	set("tls-key", cmds.userd.standaloneKey, config.Listen.TLSKey)

	var keys, users []string
	for _, user := range config.Users {
		keys = append(keys, user.Keys...)
		if user.Password != "" {
			users = append(users, user.Username+":"+user.Password)
		}
	}
	*cmds.userd.standaloneKeys = append(keys, *cmds.userd.standaloneKeys...)
	*cmds.userd.standaloneUsers = append(users, *cmds.userd.standaloneUsers...)

	if *cmds.mode != serveModeStandalone && (config.Listen != serveConfigListen{} || len(config.Users) > 0) {
		log.Warnf("config: listen and users are only used in standalone mode, not in %s mode", *cmds.mode)
	}
}
