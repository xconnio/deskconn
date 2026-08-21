package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/godbus/dbus/v5"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn"
	sysinfo "github.com/xconnio/deskconn/info"
	"github.com/xconnio/xconn-go"
)

var version = "v0.1.0-alpha"

// cliAliasScript describes a short subcommand-specific wrapper installed
// alongside deskconn, e.g. "dsh" running "deskconn shell".
type cliAliasScript struct {
	name       string
	subcommand []string
}

// aliasScripts lists the wrapper scripts installed next to the deskconn binary.
func aliasScripts() []cliAliasScript {
	return []cliAliasScript{
		{name: aliasProgShell, subcommand: []string{subcommandShell}},
		{name: aliasProgCopy, subcommand: []string{subcommandFile, subcommandCopy}},
	}
}

// aliasWrapperScript renders a POSIX sh wrapper that runs deskconn with
// subcommand fixed, forwarding the rest of argv untouched. It also forwards
// live completion requests ("--completion-bash", the first arg per
// kingpin's generated completion script) so tab-completion still works.
func aliasWrapperScript(subcommand []string) string {
	args := strings.Join(subcommand, " ")
	return fmt.Sprintf(`#!/bin/sh
if [ "$1" = "%[1]s" ]; then
    shift
    exec deskconn %[1]s %[2]s "$@"
fi
exec deskconn %[2]s "$@"
`, completionBashFlag, args)
}

// cliAliases lists every extra binary name installed alongside deskconn:
// "desk" is a plain symlink, the rest are wrapper scripts from aliasScripts.
func cliAliases() []string {
	names := []string{"desk"}
	for _, s := range aliasScripts() {
		names = append(names, s.name)
	}
	return names
}

const (
	ModeQUIC = "quic"
	ModeP2P  = "p2p"

	jsonFieldPath = "path"

	aliasProgShell = "dsh"
	aliasProgCopy  = "dcp"

	subcommandShell = "shell"
	subcommandFile  = "file"
	subcommandCopy  = "cp"

	completionBashFlag = "--completion-bash"
)

func main() {
	cfgDirectory, err := deskconn.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	versionString := fmt.Sprintf("deskconn %s", version)
	app := kingpin.New("deskconn", "Deskconn control CLI")

	attachCmd := app.Command("attach", "Attach a device")
	attachName := attachCmd.Flag("name", "Device name").Short('n').String()
	attachUsername := attachCmd.Flag("username", "Username").Short('u').String()
	attachPassword := attachCmd.Flag("password", "Password").Short('p').String()
	attachPasswordStdin := attachCmd.Flag("password-stdin", "Read password from stdin").Bool()

	detachCmd := app.Command("detach", "Detach device")
	detachUsername := detachCmd.Flag("username", "Username").Short('u').String()
	detachPassword := detachCmd.Flag("password", "Password").Short('p').String()
	detachPasswordStdin := detachCmd.Flag("password-stdin", "Read password from stdin").Bool()

	loginCmd := app.Command("login", "Login and store credentials")
	loginUsername := loginCmd.Flag("username", "Username").Short('u').String()
	loginPassword := loginCmd.Flag("password", "Password").Short('p').String()
	loginPasswordStdin := loginCmd.Flag("password-stdin", "Read password from stdin").Bool()

	fileCmd := app.Command("file", "File operations")

	lsFileCmd := fileCmd.Command("ls", "List files on a device")
	lsFileTarget := lsFileCmd.Arg("target", "Remote path as device:path (e.g. m1:/tmp)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	lsFileModeFlag := lsFileCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	mvCmd := fileCmd.Command("mv", "Move or rename a file or directory on a device")
	mvSrc := mvCmd.Arg("src", "Source path as device:path (e.g. m1:/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	mvDst := mvCmd.Arg("dst", "Destination path as device:path (e.g. m1:/b.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	mvModeFlag := mvCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	cpCmd := fileCmd.Command("cp", "Copy files to/from/between devices")
	cpSrc := cpCmd.Arg("src", "Source: device:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cpDst := cpCmd.Arg("dst", "Destination: device:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cpRecursive := cpCmd.Flag("recursive", "Copy directories recursively").Short('r').Bool()
	cpModeFlag := cpCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	rmCmd := fileCmd.Command("rm", "Remove a file or directory on a device")
	rmTarget := rmCmd.Arg("target", "Remote path as device:path (e.g. m1:/tmp/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	rmModeFlag := rmCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	catCmd := fileCmd.Command("cat", "Print the contents of a file on a device")
	catTarget := catCmd.Arg("target", "Remote path as device:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	catModeFlag := catCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	editCmd := fileCmd.Command("edit", "Edit a text file on a device with $EDITOR and send only the diff")
	editTarget := editCmd.Arg("target", "Remote path as device:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	editModeFlag := editCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	shellCmd := app.Command("shell", "Start interactive shell")
	shellDeviceName := shellCmd.Arg("device", "ID, name or alias of device to shell").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	shellModeFlag := shellCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)
	shellForwardAgent := shellCmd.Flag("agent-forward",
		"Forward the local SSH agent to the remote shell (like ssh -A). Only use with hosts you trust.",
	).Short('A').Bool()

	execCmd := app.Command("exec", "Run a command")
	execDeviceName := execCmd.Arg("device", "ID, name or alias of device to run command").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	command := execCmd.Arg("command", "Command to run").Required().Strings()
	execModeFlag := execCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	printCmd := app.Command("print", "Print operations")
	printEnableFlag := printCmd.Flag("enable", "Enable receiving print jobs on this desktop").Bool()
	printHostPrintersFlag := printCmd.Flag("host-printers",
		"Also allow remote clients to list this desktop's printers (use with --enable)").Bool()
	printDisableFlag := printCmd.Flag("disable", "Disable receiving print jobs on this desktop").Bool()
	printStatusFlag := printCmd.Flag("status", "Show whether this desktop accepts print jobs").Bool()
	printLsDevice := printCmd.Flag("ls", "List printers on a device (device name or alias)").
		HintAction(deviceCompletions(cfgDirectory)).String()
	printTarget := printCmd.Arg("target", "Device and printer as machine:printer (e.g. m1:HP_LaserJet)").
		HintAction(devicePathCompletions(cfgDirectory)).String()
	printFilePath := printCmd.Arg("file_path", "Local file path").String()
	printModeFlag := printCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	portCmd := app.Command("port", "Port forwarding operations")
	portForwardCmd := portCmd.Command("forward", "Forward a local port to a port on the remote device")
	portForwardDevice := portForwardCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	portForwardPorts := portForwardCmd.Arg("ports", "Port mapping as localport:remoteport").String()
	portForwardLocalFlag := portForwardCmd.Flag("local", "Local port to listen on").Short('l').String()
	portForwardRemoteFlag := portForwardCmd.Flag("remote", "Port on the remote device to connect to").Short('r').String()
	portForwardModeFlag := portForwardCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	portReverseCmd := portCmd.Command("reverse", "Reverse-forward a remote port to a local port")
	portReverseDevice := portReverseCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	portReversePorts := portReverseCmd.Arg("ports", "Port mapping as remoteport:localport").String()
	portReverseRemoteFlag := portReverseCmd.Flag("remote", "Port on the remote device to listen on").Short('r').String()
	portReverseLocalFlag := portReverseCmd.Flag("local", "Local port to connect to").Short('l').String()
	portReverseModeFlag := portReverseCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	pingCmd := app.Command("ping", "Ping a device and measure round-trip time")
	pingDevice := pingCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	pingCount := pingCmd.Flag("count", "Number of pings to send (0 = infinite)").Short('c').Default("0").Int()

	connectCmd := app.Command("connect", "Establish a persistent connection to a device")
	connectDevice := connectCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()

	disconnectCmd := app.Command("disconnect", "Disconnect a persistent connection")
	disconnectDevice := disconnectCmd.Arg("device", "ID, name or alias of device").
		HintAction(deviceCompletions(cfgDirectory)).String()
	disconnectAllFlag := disconnectCmd.Flag("all", "Disconnect all devices").Bool()

	lsCmd := app.Command("ls", "List devices")
	lsRefreshFlag := lsCmd.Flag("refresh", "Refresh device list from cloud").Bool()
	lsDetailedFlag := lsCmd.Flag("detailed", "Show detailed output").Bool()

	whoamiCMD := app.Command("whoami", "Show current user")

	logoutCmd := app.Command("logout", "Logout")

	configCmd := app.Command("config", "Manage deskconn configuration")
	configShow := configCmd.Command("show", "Show config")
	configSet := configCmd.Command("set", "Set device alias")
	configSetDevice := configSet.Arg("device", "ID, name or alias of device").Required().String()
	configSetKey := configSet.Arg("key", "Config key to set").Required().String()
	configSetValue := configSet.Arg("value", "Config value").Required().String()
	configUnset := configCmd.Command("unset", "Unset device alias")
	configUnsetDevice := configUnset.Arg("device", "ID, name or alias of device").Required().String()
	configUnsetKey := configUnset.Arg("key", "Config key").Required().String()
	configEdit := configCmd.Command("edit", "Edit full config")

	infoCmd := app.Command("info", "Show device resource usage")
	infoDevice := infoCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	infoModeFlag := infoCmd.Flag("mode",
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC",
	).Enum(ModeQUIC, ModeP2P)

	logsCmd := app.Command("logs", "Stream logs from a device")
	logsDevice := logsCmd.Arg("device", "ID, name or alias of device").Required().
		HintAction(deviceCompletions(cfgDirectory)).String()
	logsSource := logsCmd.Arg("source",
		"Service name (e.g. nginx) or file path (e.g. /var/log/syslog); omit for system journal").String()
	logsFollow := logsCmd.Flag("follow", "Follow log output").Short('f').Bool()
	logsTail := logsCmd.Flag("tail", "Number of lines to show from the end (-1 = default)").Short('n').
		Default("-1").Int64()
	logsSince := logsCmd.Flag("since", "Show entries since duration ago (e.g. 1h, 30m)").String()

	screenshotCmd := app.Command("screenshot", "Screenshot settings")
	screenshotEnableCmd := screenshotCmd.Command("enable", "Allow remote screenshot access")
	screenshotDisableCmd := screenshotCmd.Command("disable", "Deny remote screenshot access")

	selfCmd := app.Command("self", "Manage the installed deskconn CLI.")
	selfVersionCmd := selfCmd.Command("version", "Show the installed deskconn version")
	selfUpdateCmd := selfCmd.Command("update", "Check for updates and install the latest release")
	selfRemoveCmd := selfCmd.Command("remove", "Remove deskconn from this machine")
	selfRemoveYes := selfRemoveCmd.Flag("yes", "Do not prompt for confirmation").Short('y').Bool()

	aiCmds := registerAICommands(app, cfgDirectory)

	if len(os.Args) == 2 && os.Args[1] == "self" {
		app.Usage([]string{"self"})
		return
	}

	parsedCmd, parseErr := app.Parse(os.Args[1:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n\n", parseErr)
		ctx, _ := app.ParseContext(os.Args[1:])
		if ctx != nil {
			_ = app.UsageForContext(ctx)
		} else {
			app.Usage(os.Args[1:])
		}
		os.Exit(1)
	}

	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)

	switch parsedCmd {
	case selfVersionCmd.FullCommand():
		fmt.Println(versionString)

	case attachCmd.FullCommand():
		if err := attach(*attachUsername, *attachPassword, *attachName, *attachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case detachCmd.FullCommand():
		if err := detach(*detachUsername, *detachPassword, *detachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case loginCmd.FullCommand():
		if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
			_, _, credErr := deskconn.ReadCredentials(cfgDirectory)
			if errors.Is(credErr, deskconn.ErrKeyExpired) {
				_ = deskconn.RemoveCredentialsFiles(cfgDirectory)
			} else {
				fmt.Fprintln(os.Stderr, "you are already logged in, please logout first")
				return
			}
		}

		if err := login(*loginUsername, *loginPassword, *loginPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}
		callResp := session.Call(deskconn.ProcedureLogin).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case lsFileCmd.FullCommand():
		device, path := parseDevicePath(*lsFileTarget)
		realm, err := deviceRealm(device, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		args := []string{"ls"}
		if path != "" {
			args = append(args, path)
		}
		switch *lsFileModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			if err := deskconn.StartInteractiveCommand(quicSess.Session, "", deskconn.ProcedureExec, args...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			if err := deskconn.StartInteractiveCommand(p2pSess, "", deskconn.ProcedureExec, args...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			if err := deskconn.StartInteractiveCommand(localSession, realm, deskconn.ProcedureProxyExec, args...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case mvCmd.FullCommand():
		srcDevice, srcPath := parseDevicePath(*mvSrc)
		dstDevice, dstPath := parseDevicePath(*mvDst)
		if srcDevice != dstDevice {
			fmt.Fprintln(os.Stderr, "cross-device move not supported")
			return
		}
		realm, err := deviceRealm(srcDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		payload, _ := json.Marshal(map[string]string{"old_path": srcPath, "new_path": dstPath})
		err = fileOp(context.Background(), uri, realm, cfgDirectory, deskconn.ProcedureFileRename, payload, *mvModeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case cpCmd.FullCommand():
		srcRemote := isRemotePath(*cpSrc)
		dstRemote := isRemotePath(*cpDst)
		switch {
		case srcRemote && dstRemote:
			srcDevice, srcPath := parseDevicePath(*cpSrc)
			dstDevice, dstPath := parseDevicePath(*cpDst)
			if srcDevice != dstDevice {
				fmt.Fprintln(os.Stderr, "cross-device copy not supported")
				return
			}
			realm, err := deviceRealm(srcDevice, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			payload, _ := json.Marshal(map[string]string{"src": srcPath, "dst": dstPath})
			err = fileOp(context.Background(), uri, realm, cfgDirectory, deskconn.ProcedureFileCopy, payload, *cpModeFlag)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case !srcRemote && dstRemote:
			dstDevice, dstPath := parseDevicePath(*cpDst)
			realm, err := deviceRealm(dstDevice, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			switch *cpModeFlag {
			case ModeQUIC:
				quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.PushFilesQUIC(quicSess, *cpSrc, dstPath, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			case ModeP2P:
				p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Leave() }()
				if err := deskconn.PushFiles(p2pSess, *cpSrc, dstPath, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			default:
				localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				if err := deskconn.PushFilesViaProxy(localSession, realm, *cpSrc, dstPath, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		case srcRemote && !dstRemote:
			srcDevice, srcPath := parseDevicePath(*cpSrc)
			realm, err := deviceRealm(srcDevice, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			switch *cpModeFlag {
			case ModeQUIC:
				quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.PullFilesQUIC(quicSess, srcPath, *cpDst, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			case ModeP2P:
				p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Leave() }()
				if err := deskconn.PullFiles(p2pSess, srcPath, *cpDst, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			default:
				localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				if err := deskconn.PullFilesViaProxy(localSession, realm, srcPath, *cpDst, *cpRecursive); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		default:
			fmt.Fprintln(os.Stderr, "at least one of src or dst must be a remote path (device:path)")
		}

	case rmCmd.FullCommand():
		device, path := parseDevicePath(*rmTarget)
		realm, err := deviceRealm(device, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		payload, _ := json.Marshal(map[string]string{jsonFieldPath: path})
		err = fileOp(context.Background(), uri, realm, cfgDirectory, deskconn.ProcedureFileDelete, payload, *rmModeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case catCmd.FullCommand():
		device, path := parseDevicePath(*catTarget)
		if path == "" {
			fmt.Fprintln(os.Stderr, "path is required")
			return
		}
		realm, err := deviceRealm(device, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		switch *catModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			if err := deskconn.CatFile(quicSess.Session, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			if err := deskconn.CatFile(p2pSess, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			if err := deskconn.CatFileViaProxy(localSession, realm, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case editCmd.FullCommand():
		device, path := parseDevicePath(*editTarget)
		if path == "" {
			fmt.Fprintln(os.Stderr, "path is required")
			return
		}
		if !deskconn.IsEditableExtension(path) {
			fmt.Fprintf(os.Stderr, "%s: not a text file, editing is not supported\n", path)
			return
		}
		realm, err := deviceRealm(device, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var original []byte
		switch *editModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			original, err = deskconn.ReadFile(quicSess.Session, path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			original, err = deskconn.ReadFile(p2pSess, path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			original, err = deskconn.ReadFileViaProxy(localSession, realm, path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		}

		tmpFile, err := os.CreateTemp("", "deskconn-edit-*-"+filepath.Base(path))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		tmpPath := tmpFile.Name()
		defer func() { _ = os.Remove(tmpPath) }()

		if _, err := tmpFile.Write(original); err != nil {
			_ = tmpFile.Close()
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := tmpFile.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}

		editCmdExec := exec.Command(editor, tmpPath) // nolint: gosec
		editCmdExec.Stdin = os.Stdin
		editCmdExec.Stdout = os.Stdout
		editCmdExec.Stderr = os.Stderr
		if err := editCmdExec.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		edited, err := os.ReadFile(tmpPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if bytes.Equal(original, edited) {
			fmt.Println("no changes made")
			return
		}

		patch := deskconn.BuildEditPatch(original, edited)
		payload, _ := json.Marshal(map[string]string{jsonFieldPath: path, "patch": patch})
		if err := fileOp(context.Background(), uri, realm, cfgDirectory, deskconn.ProcedureFileEdit,
			payload, *editModeFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("file updated")

	case shellCmd.FullCommand():
		realm, err := deviceRealm(*shellDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var agentSock string
		if *shellForwardAgent {
			agentSock = os.Getenv("SSH_AUTH_SOCK")
			if agentSock == "" {
				fmt.Fprintln(os.Stderr, "warning: SSH_AUTH_SOCK not set, continuing without agent forwarding")
			}
		}

		switch *shellModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			defer setupAgentForward(quicSess.Session, "", agentSock)()
			if err := deskconn.StartInteractiveCommand(quicSess.Session, "", deskconn.ProcedureShell); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			defer setupAgentForward(p2pSess, "", agentSock)()
			if err := deskconn.StartInteractiveCommand(p2pSess, "", deskconn.ProcedureShell); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer setupAgentForward(localSession, realm, agentSock)()
			if err := deskconn.StartInteractiveCommand(localSession, realm,
				deskconn.ProcedureProxyShell); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case execCmd.FullCommand():
		realm, err := deviceRealm(*execDeviceName, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		switch *execModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			if err := deskconn.StartInteractiveCommand(quicSess.Session, "", deskconn.ProcedureExec, *command...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			if err := deskconn.StartInteractiveCommand(p2pSess, "", deskconn.ProcedureExec, *command...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			if err := deskconn.StartInteractiveCommand(localSession, realm,
				deskconn.ProcedureProxyExec, *command...); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case printCmd.FullCommand():
		switch {
		case *printEnableFlag:
			if *printHostPrintersFlag {
				if err := deskconn.EnablePrinterHosting(); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				fmt.Println("printer hosting enabled")
			} else {
				if err := deskconn.EnablePrinting(); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				fmt.Println("printing enabled")
			}
		case *printDisableFlag:
			if err := deskconn.DisablePrinting(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			fmt.Println("printing disabled")
		case *printStatusFlag:
			mode, err := deskconn.CurrentPrintMode()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			switch mode {
			case deskconn.PrintModeDisabled:
				fmt.Println("printing disabled")
			case deskconn.PrintModeAccept:
				fmt.Println("printing enabled")
			case deskconn.PrintModeHost:
				fmt.Println("printing enabled; printer hosting enabled")
			default:
				fmt.Printf("printing mode: %s\n", mode)
			}
		case *printLsDevice != "":
			realm, err := deviceRealm(*printLsDevice, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			var callResp xconn.CallResponse
			switch *printModeFlag {
			case ModeQUIC:
				quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Connection().Close()
				callResp = quicSess.Session.Call(deskconn.ProcedurePrinterList).Do()
			case ModeP2P:
				p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Leave() }()
				callResp = p2pSess.Call(deskconn.ProcedurePrinterList).Do()
			default:
				localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				callResp = localSession.Call(deskconn.ProcedureProxyPrinterList).Args(realm).Do()
			}
			if callResp.Err != nil {
				fmt.Fprintln(os.Stderr, callResp.Err)
				return
			}
			if len(callResp.Args()) == 0 {
				return
			}
			jsonData, err := json.Marshal(callResp.Args()[0])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			var printers []deskconn.PrinterInfo
			if err := json.Unmarshal(jsonData, &printers); err != nil {
				fmt.Fprintln(os.Stderr, "expected a list of printers")
				return
			}
			table := tablewriter.NewWriter(os.Stdout)
			table.Header([]string{"NAME", "PPD"})
			for _, printerInfo := range printers {
				_ = table.Append([]any{printerInfo.Name, printerInfo.PPDModel})
			}
			if err = table.Render(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case *printTarget != "":
			device, printerName := parseDevicePath(*printTarget)
			if printerName == "" {
				fmt.Fprintln(os.Stderr, "invalid target: expected machine:printer (e.g. m1:HP_LaserJet)")
				return
			}
			if *printFilePath == "" {
				fmt.Fprintln(os.Stderr, "file_path required")
				return
			}
			data, err := os.ReadFile(*printFilePath)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			filename := filepath.Base(*printFilePath)
			realm, err := deviceRealm(device, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			var callResp xconn.CallResponse
			switch *printModeFlag {
			case ModeQUIC:
				quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Connection().Close()
				callResp = quicSess.Session.Call(deskconn.ProcedurePrinterPrint).Args(printerName, filename, data).Do()
			case ModeP2P:
				p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Leave() }()
				callResp = p2pSess.Call(deskconn.ProcedurePrinterPrint).Args(printerName, filename, data).Do()
			default:
				localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				callResp = localSession.Call(deskconn.ProcedureProxyPrinterPrint).Args(realm, printerName, filename, data).Do()
			}
			if callResp.Err != nil {
				fmt.Fprintln(os.Stderr, callResp.Err)
				return
			}
			if len(callResp.Args()) > 0 {
				fmt.Printf("print job queued: %v\n", callResp.Args()[0])
			} else {
				fmt.Println("print job queued")
			}
		default:
			app.Usage([]string{"print"})
		}

	case portForwardCmd.FullCommand():
		var localPort, remotePort string
		if *portForwardPorts != "" && (*portForwardLocalFlag != "" || *portForwardRemoteFlag != "") {
			fmt.Fprintln(os.Stderr, "specify ports with either localport:remoteport or --local and --remote, not both")
			return
		}

		if *portForwardPorts != "" {
			parts := strings.SplitN(*portForwardPorts, ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				fmt.Fprintln(os.Stderr, "invalid port mapping: use localport:remoteport")
				return
			}
			localPort, remotePort = parts[0], parts[1]
		} else if *portForwardLocalFlag != "" && *portForwardRemoteFlag != "" {
			localPort = *portForwardLocalFlag
			remotePort = *portForwardRemoteFlag
		} else if *portForwardLocalFlag != "" || *portForwardRemoteFlag != "" {
			fmt.Fprintln(os.Stderr, "both --local(-l) and --remote(-r) must be provided together")
			return
		} else {
			fmt.Fprintln(os.Stderr, "specify ports as localport:remoteport or use --local and --remote flags")
			return
		}

		realm, err := deviceRealm(*portForwardDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("Forwarding 127.0.0.1:%s -> %s:%s\n", localPort, *portForwardDevice, remotePort)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		errCh := make(chan error, 1)
		switch *portForwardModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			deskconn.SafeGo(func() { errCh <- deskconn.ForwardLocalPort(ctx, quicSess.Session, remotePort, localPort) })
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			deskconn.SafeGo(func() { errCh <- deskconn.ForwardLocalPort(ctx, p2pSess, remotePort, localPort) })
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			deskconn.SafeGo(func() {
				resp := localSession.Call(deskconn.ProcedureProxyPortForward).
					Args(realm, remotePort, localPort).DoContext(ctx)
				errCh <- resp.Err
			})
		}

		select {
		case <-sigCh:
			fmt.Println("\nStopping port forwarding...")
			cancel()
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case portReverseCmd.FullCommand():
		var remotePort, localPort string
		if *portReversePorts != "" && (*portReverseRemoteFlag != "" || *portReverseLocalFlag != "") {
			fmt.Fprintln(os.Stderr, "specify ports with either remoteport:localport or --remote and --local, not both")
			return
		}

		if *portReversePorts != "" {
			parts := strings.SplitN(*portReversePorts, ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				fmt.Fprintln(os.Stderr, "invalid port mapping: use remoteport:localport")
				return
			}
			remotePort, localPort = parts[0], parts[1]
		} else if *portReverseRemoteFlag != "" && *portReverseLocalFlag != "" {
			remotePort = *portReverseRemoteFlag
			localPort = *portReverseLocalFlag
		} else if *portReverseRemoteFlag != "" || *portReverseLocalFlag != "" {
			fmt.Fprintln(os.Stderr, "both --remote(-r) and --local(-l) must be provided together")
			return
		} else {
			fmt.Fprintln(os.Stderr, "specify ports as remoteport:localport or use --remote and --local flags")
			return
		}

		realm, err := deviceRealm(*portReverseDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("Reverse forwarding %s:%s -> 127.0.0.1:%s\n", *portReverseDevice, remotePort, localPort)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		errCh := make(chan error, 1)
		switch *portReverseModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			deskconn.SafeGo(func() { errCh <- deskconn.ReverseLocalPort(ctx, quicSess.Session, remotePort, localPort) })
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			deskconn.SafeGo(func() { errCh <- deskconn.ReverseLocalPort(ctx, p2pSess, remotePort, localPort) })
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			deskconn.SafeGo(func() {
				resp := localSession.Call(deskconn.ProcedureProxyPortReverse).
					Args(realm, remotePort, localPort).DoContext(ctx)
				errCh <- resp.Err
			})
		}

		select {
		case <-sigCh:
			fmt.Println("\nStopping reverse port forwarding...")
			cancel()
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case lsCmd.FullCommand():
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}

		devicesCallResp := session.Call(deskconn.ProcedureConnectedDevices).Do()
		if devicesCallResp.Err != nil {
			fmt.Fprintln(os.Stderr, devicesCallResp.Err)
			return
		}
		connectedDevices, ok := devicesCallResp.Args()[0].(map[string]any)
		if !ok {
			fmt.Fprintln(os.Stderr, "expected a map of strings")
			return
		}
		fileExists := true
		devicesFromCfg, err := deskconn.DevicesFromCfg(cfgDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fileExists = false
			} else {
				if !*lsRefreshFlag {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				fmt.Println("your config is invalid, refreshing from cloud")
			}
		}
		tableHeader := []string{"ID", "NAME", "ALIAS", "CONNECTED SINCE"}
		if *lsDetailedFlag {
			tableHeader = append(tableHeader, "ORGANIZATION", "REALM")
		}
		if !*lsRefreshFlag && fileExists {
			table := tablewriter.NewWriter(os.Stdout)
			table.Header(tableHeader)
			for _, d := range devicesFromCfg {
				if d.Name == "" && d.Authid == "" && d.Alias == "" {
					continue
				}
				since := connectedSince(connectedDevices, d.Realm)
				if *lsDetailedFlag {
					_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since, d.Organization.Name, d.Realm})
				} else {
					_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since})
				}
			}

			if err = table.Render(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			return
		}

		devices, err := deskconn.FetchDevicesFromCloud(cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.Header(tableHeader)

		cfgMap := make(map[string]deskconn.Device, len(devicesFromCfg))
		for _, d := range devicesFromCfg {
			cfgMap[d.Authid] = d
		}

		nameCount := make(map[string]int)
		for i, d := range devices {
			count := nameCount[d.Name]
			if count > 0 {
				newName := fmt.Sprintf("%s-%s", d.Name, d.Authid[:6])
				devices[i].Name = newName
			}
			if localDevice, ok := cfgMap[d.Authid]; ok {
				devices[i].Alias = localDevice.Alias
			}
			d = devices[i]
			nameCount[d.Name]++
			since := connectedSince(connectedDevices, d.Realm)
			if *lsDetailedFlag {
				_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since, d.Organization.Name, d.Realm})
			} else {
				_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since})
			}
		}

		if err = table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		b, err := yaml.Marshal(deskconn.Config{Devices: devices})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), b, 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case whoamiCMD.FullCommand():
		path := filepath.Join(cfgDirectory, "id_ed25519.pub")

		credentialsStr, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "user not logged in")
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		credentials := strings.Split(strings.TrimSpace(string(credentialsStr)), " ")

		if len(credentials) > 2 {
			fmt.Printf("Logged in as %s (%s).\n", credentials[2], credentials[1])
		} else {
			fmt.Printf("Logged in as %s.\n", credentials[1])
		}

	case logoutCmd.FullCommand():
		if err := logout(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}

		callResp := session.Call(deskconn.ProcedureLogout).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case pingCmd.FullCommand():
		realm, err := deviceRealm(*pingDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("PING %s\n", *pingDevice)

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

		var sent, received int
		var totalMs int64
		var minMs, maxMs int64 = 1 << 62, 0

		for seq := 1; *pingCount == 0 || seq <= *pingCount; seq++ {
			select {
			case <-sigCh:
				goto pingDone
			default:
			}

			callResp := localSession.Call(deskconn.ProcedureProxyPing).Args(realm).Do()
			sent++
			if callResp.Err != nil {
				fmt.Fprintf(os.Stderr, "seq=%d error: %v\n", seq, callResp.Err)
			} else {
				ms, _ := callResp.ArgInt64(0)
				received++
				totalMs += ms
				if ms < minMs {
					minMs = ms
				}
				if ms > maxMs {
					maxMs = ms
				}
				fmt.Printf("seq=%d time=%dms\n", seq, ms)
			}

			if *pingCount == 0 || seq < *pingCount {
				select {
				case <-sigCh:
					goto pingDone
				case <-time.After(time.Second):
				}
			}
		}

	pingDone:
		loss := 0
		if sent > 0 {
			loss = (sent - received) * 100 / sent
		}
		fmt.Printf("\n--- %s ping statistics ---\n", *pingDevice)
		fmt.Printf("%d packets transmitted, %d received, %d%% packet loss\n", sent, received, loss)
		if received > 0 {
			fmt.Printf("rtt min/avg/max = %d/%d/%d ms\n", minMs, totalMs/int64(received), maxMs)
		}

	case connectCmd.FullCommand():
		realm, err := deviceRealm(*connectDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		callResp := session.Call(deskconn.ProcedureConnect).Args(realm).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		fmt.Printf("connected to %s\n", *connectDevice)

	case disconnectCmd.FullCommand():
		session, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if *disconnectAllFlag {
			callResp := session.Call(deskconn.ProcedureDisconnectAll).Do()
			if callResp.Err != nil {
				fmt.Fprintln(os.Stderr, callResp.Err)
				return
			}
			fmt.Println("disconnected all devices")
			return
		}
		if *disconnectDevice == "" {
			fmt.Fprintln(os.Stderr, "specify a device or use --all")
			return
		}
		realm, err := deviceRealm(*disconnectDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		callResp := session.Call(deskconn.ProcedureDisconnect).Args(realm).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		fmt.Printf("disconnected %s\n", *disconnectDevice)

	case configShow.FullCommand():
		data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "no config found, user not logged in")
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Print(string(data))

	case configSet.FullCommand():
		if *configSetKey != "alias" {
			fmt.Fprintln(os.Stderr, "unsupported key to set")
		}
		if err := updateDeviceAlias(cfgDirectory, *configSetDevice, *configSetValue); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case configUnset.FullCommand():
		if *configUnsetKey != "alias" {
			fmt.Fprintln(os.Stderr, "unsupported key to unset")
		}
		if err := updateDeviceAlias(cfgDirectory, *configUnsetDevice, ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case configEdit.FullCommand():
		_, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "no config found, user not logged in")
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}

		cmd := exec.Command(editor, filepath.Join(cfgDirectory, "config.yml")) // nolint: gosec
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()

	case infoCmd.FullCommand():
		realm, err := deviceRealm(*infoDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var callResp xconn.CallResponse
		switch *infoModeFlag {
		case ModeQUIC:
			quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			callResp = quicSess.Call(deskconn.ProcedureDeviceInfo).Do()
		case ModeP2P:
			p2pSess, err := deskconn.ConnectDeviceRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			callResp = p2pSess.Call(deskconn.ProcedureDeviceInfo).Do()
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			callResp = localSession.Call(deskconn.ProcedureProxyDeviceInfo).Args(realm).Do()
		}

		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		rawData, err := callResp.ArgBytes(0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		var info sysinfo.DeviceInfo
		if err := json.Unmarshal(rawData, &info); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		t := info.CPUTimes
		fmt.Printf("CPU Model:  %s\n", info.CPUModel)
		fmt.Printf("CPU Cores:  %d physical, %d logical\n", info.CPUPhysical, info.CPULogical)
		const cols = 4
		for i, usage := range info.CPUUsages {
			fmt.Printf("  CPU%-3d %5.1f%%", i, usage)
			if (i+1)%cols == 0 || i == len(info.CPUUsages)-1 {
				fmt.Println()
			}
		}
		fmt.Printf("%%Cpu(s):    %5.1f us, %5.1f sy, %5.1f ni, %5.1f id, %5.1f wa, %5.1f hi, %5.1f si, %5.1f st\n",
			t.User, t.System, t.Nice, t.Idle, t.IOWait, t.IRQ, t.SoftIRQ, t.Steal)
		fmt.Printf("MiB Mem:   %8.1f total, %8.1f free, %8.1f used, %8.1f buff/cache, %8.1f avail\n",
			toMiB(info.RAMTotal), toMiB(info.RAMFree), toMiB(info.RAMUsed),
			toMiB(info.RAMBuffCache), toMiB(info.RAMAvailable))
		fmt.Printf("MiB Swap:  %8.1f total, %8.1f free, %8.1f used\n",
			toMiB(info.SwapTotal), toMiB(info.SwapFree), toMiB(info.SwapUsed))
		fmt.Printf("Disk (/):   %s used, %s free / %s total\n",
			formatBytes(info.DiskUsed), formatBytes(info.DiskFree), formatBytes(info.DiskTotal))
		if b := info.Battery; b != nil {
			fmt.Printf("Battery:    %s (%d%%)", b.Status, b.Percentage)
			if b.TimeRemainingMins > 0 {
				fmt.Printf(", %dh%02dm remaining", b.TimeRemainingMins/60, b.TimeRemainingMins%60)
			}
			fmt.Println()
		}

	case logsCmd.FullCommand():
		realm, err := deviceRealm(*logsDevice, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(), uri, deskconn.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		source := ""
		if logsSource != nil {
			source = *logsSource
		}
		if err := deskconn.StreamLogs(localSession, realm, source, *logsFollow, *logsTail, *logsSince); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case screenshotEnableCmd.FullCommand():
		confirmed, err := confirmPrompt(
			"Deskconn will take a screenshot to verify screenshot permission. Allow? (y/n): ", false)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if !confirmed {
			fmt.Fprintln(os.Stderr, "screenshot enable cancelled")
			return
		}

		homedir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		credFilePath := filepath.Join(homedir, ".deskconn/credentials.json")
		if _, err := os.Stat(credFilePath); err != nil {
			fmt.Fprintln(os.Stderr, "device is not attached to any account")
			return
		}

		data, err := os.ReadFile(credFilePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var creds deskconn.Credentials
		if err := json.Unmarshal(data, &creds); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		quicSess, err := deskconn.ConnectDeviceRealmQUIC(context.Background(), creds.Realm, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer quicSess.Connection().Close()
		callResp := quicSess.Session.Call(deskconn.ProcedureScreenshotPermission).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			sessionBus, err := dbus.ConnectSessionBus()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			_ = deskconn.RevokeScreenshotPermission(sessionBus)
			return
		}
		if err := deskconn.EnableScreenshot(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("screenshot enabled")

	case screenshotDisableCmd.FullCommand():
		sessionBus, err := dbus.ConnectSessionBus()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer sessionBus.Close()
		var disableErr error
		if err := deskconn.RevokeScreenshotPermission(sessionBus); err != nil {
			disableErr = errors.Join(disableErr, fmt.Errorf("revoke screenshot permission: %w", err))
		}
		if err := deskconn.DisableScreenshot(cfgDirectory); err != nil {
			disableErr = errors.Join(disableErr, err)
		}
		if disableErr != nil {
			fmt.Fprintln(os.Stderr, disableErr)
			return
		}
		fmt.Println("screenshot disabled")

	case selfUpdateCmd.FullCommand():
		if err := updateApp(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

	case selfRemoveCmd.FullCommand():
		if err := removeApp(cfgDirectory, *selfRemoveYes); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	default:
		dispatchAICommand(parsedCmd, aiCmds, cfgDirectory)
	}
}

type appUpdateResponse struct {
	DownloadURL   string `json:"download_url"`
	LatestVersion string `json:"latest_version"`
}

func updateApp() error {
	fmt.Printf("Checking for updates for %s on %s-%s...\n", version, runtime.GOOS, runtime.GOARCH)

	updateResp, err := latestAppUpdate()
	if err != nil {
		return err
	}

	if updateResp.DownloadURL == "" {
		fmt.Printf("You're already on version %s of deskconn (the latest version).\n", version)
		return nil
	}

	fmt.Printf("Found update at %s.\n", updateResp.DownloadURL)

	if err := downloadAndInstallUpdate(updateResp.DownloadURL); err != nil {
		return err
	}

	fmt.Printf("Restarting deskconnd service...\n")
	cmd := exec.Command("systemctl", "--user", "restart", "deskconnd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart deskconnd: %w", err)
	}

	fmt.Printf("Updated deskconn from version %s to %s.\n", version, updateResp.LatestVersion)
	return nil
}

func latestAppUpdate() (appUpdateResponse, error) {
	const releaseURL = "https://api.github.com/repos/xconnio/deskconn/releases/latest"

	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	req, err := http.NewRequest(http.MethodGet, releaseURL, nil)
	if err != nil {
		return appUpdateResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "deskconn-self-update")

	resp, err := client.Do(req)
	if err != nil {
		return appUpdateResponse{}, fmt.Errorf("failed to check latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return appUpdateResponse{}, fmt.Errorf("failed to check latest release: unexpected status %s", resp.Status)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return appUpdateResponse{}, fmt.Errorf("failed to parse latest release: %w", err)
	}

	if release.TagName == "" {
		return appUpdateResponse{}, fmt.Errorf("latest release response missing tag_name")
	}
	if strings.TrimPrefix(version, "v") == strings.TrimPrefix(release.TagName, "v") {
		return appUpdateResponse{LatestVersion: release.TagName}, nil
	}

	assetName := fmt.Sprintf("deskconn_%s_%s_%s.tar.gz", strings.TrimPrefix(release.TagName, "v"),
		runtime.GOOS, runtime.GOARCH)
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			if asset.BrowserDownloadURL == "" {
				return appUpdateResponse{}, fmt.Errorf("latest release asset %s missing browser_download_url", assetName)
			}
			return appUpdateResponse{
				DownloadURL:   asset.BrowserDownloadURL,
				LatestVersion: release.TagName,
			}, nil
		}
	}

	return appUpdateResponse{}, fmt.Errorf("latest release %s missing asset %s", release.TagName, assetName)
}

const pathInstallerBlock = "\n# Added by deskconn installer\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"

// agentForwardReadyTimeout bounds how long "deskconn shell -A" waits for the remote agent
// forwarding listener to come up before giving up and starting a normal (unforwarded) shell.
const agentForwardReadyTimeout = 5 * time.Second

// setupAgentForward starts SSH-agent forwarding on session (see deskconn.RunAgentForward) when
// agentSock is non-empty, and blocks until the remote listener is confirmed ready (or setup
// fails/times out) so the caller can safely start the shell right after — the remote's
// caller-keyed forwarding state must exist before the shell's first handshake message can
// trigger the PTY spawn. Forwarding failures are reported as warnings; they never prevent the
// shell itself from starting. The returned func stops forwarding and must be called (deferred)
// once the shell exits.
func setupAgentForward(session *xconn.Session, realm, agentSock string) func() {
	if agentSock == "" {
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	deskconn.SafeGo(func() {
		_ = deskconn.RunAgentForward(ctx, session, realm, agentSock, ready)
	})

	select {
	case err := <-ready:
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: agent forwarding unavailable: %v\n", err)
		}
	case <-time.After(agentForwardReadyTimeout):
		fmt.Fprintln(os.Stderr, "warning: agent forwarding setup timed out, continuing without it")
	}
	return cancel
}

// confirmPrompt prints prompt, reads a line from stdin, and reports whether the response counts
// as affirmative: "y"/"yes" (case-insensitive) are always affirmative, and an empty response
// (just Enter) falls back to defaultYes.
func confirmPrompt(prompt string, defaultYes bool) (bool, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "y", "yes":
		return true, nil
	case "":
		return defaultYes, nil
	default:
		return false, nil
	}
}

func removeApp(cfgDirectory string, skipConfirm bool) error {
	if !skipConfirm {
		confirmed, err := confirmPrompt("Are you sure you want to remove deskconn and all configuration, "+
			"credentials and device list? (y/N): ", false)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("remove cancelled")
			return nil
		}
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	const serviceName = "deskconnd"
	serviceFile := filepath.Join(homeDir, ".config", "systemd", "user", serviceName+".service")

	if _, err := os.Stat(serviceFile); err == nil {
		fmt.Println("Stopping deskconnd service...")
		_ = exec.Command("systemctl", "--user", "stop", serviceName).Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", serviceName).Run() // nolint: gosec
		if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove service file: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	}

	binDir := filepath.Join(homeDir, ".local", "bin")
	execDir := filepath.Join(homeDir, ".local", "lib", "exec")
	bashCompDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshCompDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	paths := []string{
		filepath.Join(binDir, "deskconn"),
		filepath.Join(execDir, "deskconnd"),
		filepath.Join(bashCompDir, "deskconn"),
		filepath.Join(zshCompDir, "_deskconn"),
	}
	for _, alias := range cliAliases() {
		paths = append(paths,
			filepath.Join(binDir, alias),
			filepath.Join(bashCompDir, alias),
			filepath.Join(zshCompDir, "_"+alias),
		)
	}

	fmt.Println("Removing deskconn binaries and shell completions...")
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove %s: %w", p, err)
		}
	}

	for _, rcFile := range []string{".bashrc", ".bash_profile", ".zshrc", ".profile"} {
		if err := removePathInstallerBlock(filepath.Join(homeDir, rcFile)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up %s: %v\n", rcFile, err)
		}
	}

	fmt.Println("Removing deskconn configuration, credentials and device list...")
	if err := os.RemoveAll(cfgDirectory); err != nil {
		return fmt.Errorf("failed to remove config directory: %w", err)
	}

	fmt.Println("deskconn has been removed.")
	return nil
}

// removePathInstallerBlock strips the exact PATH block install.sh appends to shell rc files.
// It is a no-op if the file doesn't exist or doesn't contain the block.
func removePathInstallerBlock(rcFile string) error {
	data, err := os.ReadFile(rcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if !strings.Contains(string(data), pathInstallerBlock) {
		return nil
	}

	updated := strings.Replace(string(data), pathInstallerBlock, "", 1)
	info, err := os.Stat(rcFile)
	if err != nil {
		return err
	}
	return os.WriteFile(rcFile, []byte(updated), info.Mode()) // nolint: gosec
}

func downloadAndInstallUpdate(downloadURL string) error {
	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	fmt.Printf("Downloading update from %s...\n", downloadURL)
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download update: unexpected status %s", resp.Status)
	}

	gzipReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to open update archive: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	fmt.Println("Extracting update archive...")

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	binDir := filepath.Join(homeDir, ".local", "bin")
	execDir := filepath.Join(homeDir, ".local", "lib", "exec")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return fmt.Errorf("failed to create bin dir: %w", err)
	}
	if err := os.MkdirAll(execDir, 0755); err != nil {
		return fmt.Errorf("failed to create exec dir: %w", err)
	}

	foundDeskconn := false
	foundDeskconnd := false

	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read update archive: %w", err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		name := filepath.Base(header.Name)
		switch name {
		case "deskconn":
			fmt.Println("Installing deskconn...")
			deskconnPath := filepath.Join(binDir, "deskconn")
			if err := installBinaryFromReader(tarReader, deskconnPath, 0755); err != nil {
				return err
			}
			deskPath := filepath.Join(binDir, "desk")
			_ = os.Remove(deskPath)
			if err := os.Symlink(deskconnPath, deskPath); err != nil {
				return fmt.Errorf("failed to create desk symlink: %w", err)
			}
			for _, s := range aliasScripts() {
				scriptPath := filepath.Join(binDir, s.name)
				script := aliasWrapperScript(s.subcommand)
				if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil { // nolint: gosec
					return fmt.Errorf("failed to write %s wrapper: %w", s.name, err)
				}
			}
			foundDeskconn = true
		case "deskconnd":
			fmt.Println("Installing deskconnd...")
			if err := installBinaryFromReader(tarReader, filepath.Join(execDir, "deskconnd"), 0700); err != nil {
				return err
			}
			foundDeskconnd = true
		}
	}

	if !foundDeskconn || !foundDeskconnd {
		return fmt.Errorf("update archive missing required binaries")
	}

	deskconnBin := filepath.Join(binDir, "deskconn")
	if err := installCompletions(deskconnBin); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update shell completions: %v\n", err)
	}

	return nil
}

func installCompletions(deskconnBin string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home dir: %w", err)
	}

	bashDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")

	for _, dir := range []string{bashDir, zshDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	bashOut, err := exec.Command(deskconnBin, "--completion-script-bash").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating bash completion: %w", err)
	}
	bashScript := fixBashCompletionSpacing(fixBashCompletionWordBreaks(string(bashOut)))
	if err := os.WriteFile(filepath.Join(bashDir, "deskconn"), []byte(bashScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}

	zshOut, err := exec.Command(deskconnBin, "--completion-script-zsh").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating zsh completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(zshDir, "_deskconn"), zshOut, 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing zsh completion: %w", err)
	}

	for _, alias := range cliAliases() {
		aliasBash := strings.Replace(bashScript,
			"complete -F _deskconn_bash_autocomplete -o default deskconn",
			"complete -F _deskconn_bash_autocomplete -o default "+alias, 1)
		if err := os.WriteFile(filepath.Join(bashDir, alias), []byte(aliasBash), 0644); err != nil { // nolint: gosec
			return fmt.Errorf("writing %s bash completion: %w", alias, err)
		}

		aliasZsh := "#compdef " + alias + "\n_deskconn \"$@\"\n"
		if err := os.WriteFile(filepath.Join(zshDir, "_"+alias), []byte(aliasZsh), 0644); err != nil { // nolint: gosec
			return fmt.Errorf("writing %s zsh completion: %w", alias, err)
		}
	}

	return nil
}

// fixBashCompletionWordBreaks clears ':' from COMP_WORDBREAKS so bash
// completes "device:path" as one word instead of splitting at the colon.
func fixBashCompletionWordBreaks(script string) string {
	const marker = "_deskconn_bash_autocomplete() {\n"
	return strings.Replace(script, marker, marker+"    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}\n", 1)
}

// fixBashCompletionSpacing makes path completions behave like "cd": the menu shows only the last
// path segment (-o filenames), and no trailing space is added after directories/"device:" prefixes.
// Leaf files are left to bash's default space-append.
func fixBashCompletionSpacing(script string) string {
	const marker = `    COMPREPLY=( $(compgen -W "${opts}" -- ${cur}) )` + "\n"
	const replacement = marker +
		"    compopt -o filenames 2>/dev/null || true\n" +
		`    if [ "${#COMPREPLY[@]}" -eq 1 ] && [[ "${COMPREPLY[0]}" == */ || "${COMPREPLY[0]}" == *: ]]; then` + "\n" +
		"        compopt -o nospace 2>/dev/null || true\n" +
		"    fi\n"
	return strings.Replace(script, marker, replacement, 1)
}

func installBinaryFromReader(src io.Reader, dst string, mode os.FileMode) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", dst, err)
	}

	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		_ = tmpFile.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmpFile, src); err != nil {
		return fmt.Errorf("failed to write %s: %w", dst, err)
	}

	if err := tmpFile.Chmod(mode); err != nil {
		return fmt.Errorf("failed to chmod %s: %w", dst, err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", dst, err)
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("failed to install %s: %w", dst, err)
	}

	cleanup = false
	return nil
}

func attach(flagUsername, flagPassword, name string, useStdin bool) error {
	file, err := deskconn.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("device already attached")
	}

	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
	if err != nil {
		return err
	}

	deviceName := name

	if deviceName == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname: %w", err)
		}

		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Name of the new device [default=%s]: ", host)

		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			deviceName = host
		} else {
			deviceName = input
		}
	}

	return deskconn.Attach(username, password, deviceName)
}

func detach(flagUsername, flagPassword string, useStdin bool) error {
	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
	if err != nil {
		return err
	}

	quicSess, err := deskconn.ConnectCloudCRA(context.Background(), username, password)
	if err != nil {
		return err
	}
	deskconn.SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	defer quicSess.Connection().Close()

	authID, name, err := selectDevice(quicSess.Session)
	if err != nil {
		return err
	}

	confirmed, err := confirmPrompt(
		fmt.Sprintf("Are you sure you want to detach desktop '%s' with ID '%s'?\n(Y/n): ", name, authID), true)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("detach cancelled")
		return nil
	}

	return deskconn.Detach(quicSess.Session, authID)
}

func login(flagUsername, flagPassword string, useStdin bool) error {
	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
	if err != nil {
		return err
	}

	quicSess, err := deskconn.ConnectCloudCRA(context.Background(), username, password)
	if err != nil {
		return err
	}
	deskconn.SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	defer quicSess.Connection().Close()

	callResp := quicSess.Call(deskconn.ProcedureAccountLogin).Args(username).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to login: %w", callResp.Err)
	}

	otp, err := readOTP()
	if err != nil {
		return err
	}

	return deskconn.Login(quicSess.Session, username, otp)
}

func logout(cfgDirectory string) error {
	cloudSession, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), deskconn.ErrAuthenticationFailed) ||
			errors.Is(err, deskconn.ErrKeyExpired) {
			return deskconn.RemoveCredentialsFiles(cfgDirectory)
		}
		return err
	}

	pubKey, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519.pub"))
	if err != nil {
		return err
	}
	pubKeyString := strings.Split(string(pubKey), " ")[0]
	callResp := cloudSession.Call(deskconn.ProcedurePrincipalDelete).Arg(pubKeyString).Do()
	if callResp.Err != nil {
		return callResp.Err
	}

	return deskconn.RemoveCredentialsFiles(cfgDirectory)
}

func deviceRealm(deviceName, cfgDirectory string) (string, error) {
	devices, err := deskconn.DevicesFromCfg(cfgDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, err := deskconn.FetchDevicesFromCloud(cfgDirectory)
			if err != nil {
				return "", err
			}

			b, err := yaml.Marshal(deskconn.Config{Devices: devices})
			if err != nil {
				return "", err
			}

			if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), b, 0600); err != nil {
				return "", err
			}

			return deviceRealm(deviceName, cfgDirectory)
		}
		return "", err
	}

	for _, d := range devices {
		if d.Name == deviceName {
			return d.Realm, nil
		}
		if d.Authid == deviceName {
			return d.Realm, nil
		}
		if d.Alias == deviceName {
			return d.Realm, nil
		}
	}

	for _, d := range devices {
		if strings.HasPrefix(d.Authid, deviceName) {
			return d.Realm, nil
		}
	}

	return "", fmt.Errorf("device not found: %s", deviceName)
}

func readCredentials(flagUsername, flagPassword string, useStdin bool) (username, password string, err error) {
	if useStdin {
		if flagUsername == "" {
			return "", "", fmt.Errorf("--username is required when using --password-stdin")
		}
		if flagPassword == "" {
			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return "", "", fmt.Errorf("reading password from stdin: %w", err)
			}
			flagPassword = strings.TrimRight(line, "\r\n")
		}
		return flagUsername, flagPassword, nil
	}

	if flagUsername == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
			return "", "", fmt.Errorf("username required: use --username flag")
		}
		fmt.Fprint(os.Stderr, "Username: ")
		reader := bufio.NewReader(os.Stdin)
		pwd, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		flagUsername = strings.TrimSpace(pwd)
		if flagUsername == "" {
			return "", "", fmt.Errorf("username cannot be empty")
		}
	}

	if flagPassword == "" {
		flagPassword, err = readPassword()
		if err != nil {
			return "", "", err
		}
	}

	return flagUsername, flagPassword, nil
}

func readOTP() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("otp required: rerun interactively to enter the one-time password emailed to you")
	}

	fmt.Fprint(os.Stderr, "OTP: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	otp := strings.TrimSpace(line)
	if otp == "" {
		return "", fmt.Errorf("otp cannot be empty")
	}

	return otp, nil
}

func readPassword() (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", fmt.Errorf("password required: use --password or --password-stdin flag")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	return string(pwd), nil
}

func selectOption(callResp xconn.CallResponse, title string, idField string, prompt string) (int, error) {
	count := len(callResp.Args())
	if count == 1 {
		return 0, nil
	}

	type row struct {
		i    int
		name string
		id   string
		line string
	}

	rows := make([]row, 0, count)
	maxWidth := 0

	for i := 0; i < count; i++ {
		dict, err := callResp.ArgDict(i)
		if err != nil {
			return -1, err
		}

		name, _ := dict.String("name")
		id, _ := dict.String(idField)

		if name == "" {
			name = id
		}

		line := fmt.Sprintf(" %2d) %-20s  %s", i+1, name, id)
		if len(line) > maxWidth {
			maxWidth = len(line)
		}

		rows = append(rows, row{i + 1, name, id, line})
	}

	sep := strings.Repeat("─", maxWidth)

	fmt.Println()
	fmt.Println(sep)
	fmt.Println(" ", title)
	fmt.Println(sep)

	for _, r := range rows {
		fmt.Println(r.line)
	}

	fmt.Println(sep)
	fmt.Printf(" %s [1-%d] (default 1): ", prompt, count)

	reader := bufio.NewReader(os.Stdin)

	for {
		input, err := reader.ReadString('\n')
		if err != nil {
			return -1, err
		}

		input = strings.TrimSpace(input)
		if input == "" {
			return 0, nil
		}

		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > count {
			fmt.Printf(" Invalid selection. Enter 1-%d: ", count)
			continue
		}

		return idx - 1, nil
	}
}

func selectDevice(session *xconn.Session) (authid string, name string, err error) {
	callResp := session.Call(deskconn.ProcedureListDesktop).Do()

	if callResp.Err != nil {
		return "", "", callResp.Err
	}
	if len(callResp.Args()) == 0 {
		return "", "", fmt.Errorf("no desktop attached to the account")
	}

	idx, err := selectOption(callResp, "Available devices", "authid", "Select device")
	if err != nil {
		return "", "", err
	}

	deviceDict, err := callResp.ArgDict(idx)
	if err != nil {
		return "", "", err
	}
	authid, err = deviceDict.String("authid")
	if err != nil {
		return "", "", err
	}
	name, err = deviceDict.String("name")
	if err != nil {
		return "", "", err
	}

	return authid, name, nil
}

func deviceCompletions(cfgDirectory string) func() []string {
	return func() []string {
		devices, err := deskconn.DevicesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range devices {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
		return names
	}
}

func devicePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		devices, err := deskconn.DevicesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range devices {
			if d.Name != "" {
				names = append(names, d.Name+":")
			}
		}
		return names
	}
}

// remotePathCompletions falls back to local filename completion when no
// "device:" prefix is typed, and browses the remote directory once one is.
func remotePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		current := currentCompletionArg()
		if !isRemotePath(current) {
			return devicePathCompletions(cfgDirectory)()
		}
		return remoteDevicePathCompletions(cfgDirectory, current)
	}
}

// currentCompletionArg returns the partial word under the cursor, passed as
// the last arg when kingpin's generated script re-invokes the binary with
// "--completion-bash".
func currentCompletionArg() string {
	if len(os.Args) == 0 {
		return ""
	}
	return os.Args[len(os.Args)-1]
}

// remoteDevicePathCompletions lists entries of the directory being typed in current.
func remoteDevicePathCompletions(cfgDirectory, current string) []string {
	device, path := parseDevicePath(current)
	realm, err := deviceRealm(device, cfgDirectory)
	if err != nil {
		return nil
	}

	dir, prefix := "", path
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		dir, prefix = path[:idx+1], path[idx+1:]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)
	localSession, err := xconn.ConnectAnonymous(ctx, uri, deskconn.LocalRealm)
	if err != nil {
		return nil
	}
	defer func() { _ = localSession.Leave() }()

	if !deviceHasPersistentSession(localSession, realm) {
		_ = localSession.Call(deskconn.ProcedureConnect).Args(realm).Do()
		return nil
	}

	browsePayload, err := json.Marshal(map[string]string{jsonFieldPath: dir})
	if err != nil {
		return nil
	}
	resp := localSession.Call(deskconn.ProcedureProxyFileOp).Args(realm, deskconn.ProcedureFileBrowse, browsePayload).Do()
	if resp.Err != nil {
		return nil
	}
	result, err := resp.ArgBytes(0)
	if err != nil {
		return nil
	}

	var browsed deskconn.FileBrowseResult
	if err := json.Unmarshal(result, &browsed); err != nil {
		return nil
	}

	var out []string
	for _, entry := range browsed.Entries {
		if !strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		completion := device + ":" + dir + entry.Name
		if entry.IsDir {
			completion += "/"
		}
		out = append(out, completion)
	}
	return out
}

func deviceHasPersistentSession(localSession *xconn.Session, realm string) bool {
	resp := localSession.Call(deskconn.ProcedureConnectedDevices).Do()
	if resp.Err != nil || len(resp.Args()) == 0 {
		return false
	}
	connected, ok := resp.Args()[0].(map[string]any)
	if !ok {
		return false
	}
	_, ok = connected[realm]
	return ok
}

func isRemotePath(s string) bool {
	return strings.ContainsRune(s, ':')
}

func parseDevicePath(s string) (device, path string) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

func fileOp(ctx context.Context, uri, realm, cfgDirectory, procedure string, payload []byte, mode string) error {
	switch mode {
	case ModeQUIC:
		quicSess, err := deskconn.ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		_, err = deskconn.CallFileOp(quicSess.Session, procedure, payload)
		return err
	case ModeP2P:
		p2pSess, err := deskconn.ConnectDeviceRealmP2P(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Leave() }()
		_, err = deskconn.CallFileOp(p2pSess, procedure, payload)
		return err
	default:
		localSession, err := xconn.ConnectAnonymous(ctx, uri, deskconn.LocalRealm)
		if err != nil {
			return err
		}
		resp := localSession.Call(deskconn.ProcedureProxyFileOp).Args(realm, procedure, payload).Do()
		return resp.Err
	}
}

func shortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[:6]
}

func connectedSince(connectedDevices map[string]any, realm string) string {
	v, ok := connectedDevices[realm]
	if !ok {
		return ""
	}
	var ts int64
	switch t := v.(type) {
	case int64:
		ts = t
	case uint64:
		ts = int64(t) // nolint: gosec
	case float64:
		ts = int64(t)
	default:
		return "connected"
	}
	return formatSince(time.Since(time.Unix(ts, 0)))
}

func formatSince(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func toMiB(b uint64) float64 {
	return float64(b) / (1024 * 1024)
}

func formatBytes(b uint64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func updateDeviceAlias(cfgDirectory, deviceKey, alias string) error {
	configFile := filepath.Join(cfgDirectory, "config.yml")

	data, err := os.ReadFile(configFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config found. User not logged in")
		}
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config deskconn.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	devices := config.Devices
	for _, d := range devices {
		if (d.Alias == alias && d.Alias != "") || d.Authid == alias || d.Name == alias {
			return fmt.Errorf("alias '%s' already in use", alias)
		}
	}

	updated := false
	for i, d := range devices {
		if d.Authid == deviceKey || d.Name == deviceKey || d.Alias == deviceKey {
			devices[i].Alias = alias
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("device %s not found", deviceKey)
	}

	out, err := yaml.Marshal(deskconn.Config{Devices: devices})
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(configFile, out, 0600)
}
