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
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	wampauth "github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

var version = "v0.1.0-alpha"

// legacyCLIPaths lists files left behind by older installs: the pre-rename "deskconn" CLI and
// the removed "dsh"/"dcp" shortcut wrappers, along with their shell completions.
func legacyCLIPaths(binDir, bashCompDir, zshCompDir string) []string {
	var paths []string
	for _, name := range []string{"deskconn", "dsh", "dcp"} {
		paths = append(paths,
			filepath.Join(binDir, name),
			filepath.Join(bashCompDir, name),
			filepath.Join(zshCompDir, "_"+name),
		)
	}
	return paths
}

const (
	ModeQUIC = "quic"
	ModeP2P  = "p2p"

	jsonFieldPath = "path"
)

func main() {
	if args, ok := vpndArgs(os.Args); ok {
		runVPNd(args)
		return
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	versionString := fmt.Sprintf("desk %s", version)
	app := kingpin.New("desk", "Deskconn control CLI")
	standaloneURL := app.Flag("url", "Connect directly to a standalone deskconnd at this URL "+
		"(quic://host:port, or https://host:port for WebTransport) instead of a machine on your account").
		Envar("DESKCONN_URL").String()
	standaloneCertHash := app.Flag("cert-hash", "SHA-256 fingerprint of the standalone machine's certificate, "+
		"as deskconnd prints it, to trust it even if self-signed (default: verify against trusted CAs)").
		Envar("DESKCONN_CERT_HASH").String()
	standaloneKey := app.Flag("private-key", "Private key (hex) to authenticate with --url; see `desk keygen`").
		Envar("DESKCONN_PRIVATE_KEY").String()
	standaloneAuthID := app.Flag("authid", "Authid (username) to present with --url (default: current user)").
		Envar("DESKCONN_AUTHID").String()
	standalonePassword := app.Flag("secret", "Password to authenticate with --url instead of "+
		"--private-key (prompted for if neither is given)").Envar("DESKCONN_SECRET").String()

	keygenCmd := app.Command("keygen", "Generate a key pair for standalone machines "+
		"(public key for deskconnd --public-key, private key for --private-key)")

	machineCmd := app.Command("machine", "Manage the machines on your account")

	machineLsCmd := machineCmd.Command("ls", "List machines")
	var lsRefresh, lsDetailed bool
	machineLsCmd.Flag("refresh", "Refresh machine list from cloud").BoolVar(&lsRefresh)
	machineLsCmd.Flag("detailed", "Show detailed output").BoolVar(&lsDetailed)

	attachCmd := machineCmd.Command("attach", "Attach this machine to your account")
	attachName := attachCmd.Flag("name", "Machine name").Short('n').String()
	attachUsername := attachCmd.Flag("username", "Username").Short('u').String()
	attachPassword := attachCmd.Flag("password", "Password").Short('p').String()
	attachPasswordStdin := attachCmd.Flag("password-stdin", "Read password from stdin").Bool()

	detachCmd := machineCmd.Command("detach", "Detach this machine from your account (asks to confirm first)")

	aliasCmd := machineCmd.Command("alias", "Set or unset a machine's alias")
	aliasMachine := aliasCmd.Arg("machine", "ID, name or alias of machine").Required().
		HintAction(machineCompletions(cfgDirectory)).String()
	aliasName := aliasCmd.Arg("name", "Alias to set").String()
	aliasUnset := aliasCmd.Flag("unset", "Remove the machine's alias").Bool()

	loginCmd := app.Command("login", "Login and store credentials")
	loginUsername := loginCmd.Flag("username", "Username").Short('u').String()
	loginPassword := loginCmd.Flag("password", "Password").Short('p').String()
	loginPasswordStdin := loginCmd.Flag("password-stdin", "Read password from stdin").Bool()

	fileCmd := app.Command("file", "File operations")

	lsFileCmd := fileCmd.Command("ls", "List files on a machine")
	lsFileTarget := lsFileCmd.Arg("target", "Remote path as machine:path (e.g. m1:/tmp)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	lsFileModeFlag := modeFlag(lsFileCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	mvCmd := fileCmd.Command("mv", "Move or rename a file or directory on a machine")
	mvSrc := mvCmd.Arg("src", "Source path as machine:path (e.g. m1:/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	mvDst := mvCmd.Arg("dst", "Destination path as machine:path (e.g. m1:/b.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	mvModeFlag := modeFlag(mvCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	cpCmd := fileCmd.Command("cp", "Copy files to/from/between machines")
	cpSrc := cpCmd.Arg("src", "Source: machine:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cpDst := cpCmd.Arg("dst", "Destination: machine:path for remote, /path for local").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	cpRecursive := cpCmd.Flag("recursive", "Copy directories recursively").Short('r').Bool()
	cpModeFlag := modeFlag(cpCmd,
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC "+
			"(default: try p2p, fall back to quic)")
	cpStreams := cpCmd.Flag("streams", "Number of parallel streams to use for the transfer (default 4)").
		Short('s').Int()

	rmCmd := fileCmd.Command("rm", "Remove a file or directory on a machine")
	rmTarget := rmCmd.Arg("target", "Remote path as machine:path (e.g. m1:/tmp/a.txt)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	rmModeFlag := modeFlag(rmCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	catCmd := fileCmd.Command("cat", "Print the contents of a file on a machine")
	catTarget := catCmd.Arg("target", "Remote path as machine:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	catModeFlag := modeFlag(catCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	editCmd := fileCmd.Command("edit", "Edit a text file on a machine with $EDITOR and send only the diff")
	editTarget := editCmd.Arg("target", "Remote path as machine:path (e.g. m1:/etc/hosts)").Required().
		HintAction(remotePathCompletions(cfgDirectory)).String()
	editModeFlag := modeFlag(editCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	shellCmd := app.Command("shell", "Start an interactive shell, or run a command if one is given")
	shellMachineName := machineArg(shellCmd, "ID, name or alias of machine", cfgDirectory)
	shellCommand := shellCmd.Arg("command", "Command to run instead of an interactive shell").Strings()
	shellModeFlag := modeFlag(shellCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")
	shellForwardAgent := shellCmd.Flag("agent-forward",
		"Forward the local SSH agent to the remote shell (like ssh -A). Only use with hosts you trust.",
	).Short('A').Bool()

	portCmd := app.Command("port", "Port forwarding operations")
	portForwardCmd := portCmd.Command("forward", "Forward a local port to a port on the remote machine")
	portForwardMachine := machineArg(portForwardCmd, "ID, name or alias of machine", cfgDirectory)
	portForwardPorts := portForwardCmd.Arg("ports", "Port mapping as localport:remoteport").String()
	portForwardLocalFlag := portForwardCmd.Flag("local", "Local port to listen on").Short('l').String()
	portForwardRemoteFlag := portForwardCmd.Flag("remote", "Port on the remote machine to connect to").Short('r').String()
	portForwardModeFlag := modeFlag(portForwardCmd,
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	portReverseCmd := portCmd.Command("reverse", "Reverse-forward a remote port to a local port")
	portReverseMachine := machineArg(portReverseCmd, "ID, name or alias of machine", cfgDirectory)
	portReversePorts := portReverseCmd.Arg("ports", "Port mapping as remoteport:localport").String()
	portReverseRemoteFlag := portReverseCmd.Flag("remote", "Port on the remote machine to listen on").Short('r').String()
	portReverseLocalFlag := portReverseCmd.Flag("local", "Local port to connect to").Short('l').String()
	portReverseModeFlag := modeFlag(portReverseCmd,
		"Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	vpnCmd := app.Command("vpn", "Route internet traffic through a remote machine, or serve as one")
	vpnConnectCmd := vpnCmd.Command("connect", "Connect to a machine and tunnel all traffic through it")
	vpnConnectMachine := machineArg(vpnConnectCmd, "ID, name or alias of machine", cfgDirectory)
	vpnServeCmd := vpnCmd.Command("serve", "Let other machines route their traffic through this machine")
	vpnStopCmd := vpnCmd.Command("stop", "Stop serving as a VPN exit node")

	pingCmd := machineCmd.Command("ping", "Ping a machine and measure round-trip time")
	pingMachine := machineArg(pingCmd, "ID, name or alias of machine", cfgDirectory)
	pingCount := pingCmd.Flag("count", "Number of pings to send (0 = infinite)").Short('c').Default("0").Int()

	connectCmd := machineCmd.Command("connect", "Establish a persistent connection to a machine")
	connectMachine := machineArg(connectCmd, "ID, name or alias of machine", cfgDirectory)

	disconnectCmd := machineCmd.Command("disconnect", "Disconnect a persistent connection")
	disconnectMachine := disconnectCmd.Arg("machine", "ID, name or alias of machine").
		HintAction(machineCompletions(cfgDirectory)).String()
	disconnectAllFlag := disconnectCmd.Flag("all", "Disconnect all machines").Bool()

	lsCmd := app.Command("ls", "List machines (shortcut for `machine ls`)")
	lsCmd.Flag("refresh", "Refresh machine list from cloud").BoolVar(&lsRefresh)
	lsCmd.Flag("detailed", "Show detailed output").BoolVar(&lsDetailed)

	whoamiCMD := app.Command("whoami", "Show current user")

	logoutCmd := app.Command("logout", "Logout")

	configCmd := app.Command("config", "Manage desk configuration")
	configShow := configCmd.Command("show", "Show config")
	configEdit := configCmd.Command("edit", "Edit full config")

	infoCmd := machineCmd.Command("info", "Show machine resource usage")
	infoMachine := machineArg(infoCmd, "ID, name or alias of machine", cfgDirectory)
	infoModeFlag := modeFlag(infoCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	logsCmd := machineCmd.Command("logs", "Stream logs from a machine")
	logsMachine := machineArg(logsCmd, "ID, name or alias of machine", cfgDirectory)
	logsSource := logsCmd.Arg("source",
		"Service name (e.g. nginx) or file path (e.g. /var/log/syslog); omit for system journal").String()
	logsFollow := logsCmd.Flag("follow", "Follow log output").Short('f').Bool()
	logsTail := logsCmd.Flag("tail", "Number of lines to show from the end (-1 = default)").Short('n').
		Default("-1").Int64()
	logsSince := logsCmd.Flag("since", "Show entries since duration ago (e.g. 1h, 30m)").String()
	logsModeFlag := modeFlag(logsCmd, "Connection mode: 'quic' uses QUIC stream via router, 'p2p' uses direct WebRTC")

	selfCmd := app.Command("self", "Manage the installed desk CLI.")
	selfVersionCmd := selfCmd.Command("version", "Show the installed desk version")
	selfUpdateCmd := selfCmd.Command("update", "Check for updates and install the latest release")
	selfRemoveCmd := selfCmd.Command("remove", "Remove desk from this machine")
	selfRemoveYes := selfRemoveCmd.Flag("yes", "Do not prompt for confirmation").Short('y').Bool()

	agentCmds := registerAgentCommands(app, cfgDirectory)

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

	if *standaloneURL != "" {
		if _, err := common.ParseStandaloneURL(*standaloneURL); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := common.StandaloneTLSConfig(*standaloneCertHash); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *standaloneKey != "" && *standalonePassword != "" {
			fmt.Fprintln(os.Stderr, "--private-key and --secret are mutually exclusive")
			os.Exit(1)
		}
		if *standaloneAuthID == "" {
			*standaloneAuthID = "deskconn"
			if u, err := user.Current(); err == nil && u.Username != "" {
				*standaloneAuthID = u.Username
			}
		}
		authID := *standaloneAuthID
		common.SetStandaloneTarget(&common.StandaloneTarget{URL: *standaloneURL, CertHash: *standaloneCertHash,
			AuthID: authID, PrivateKey: *standaloneKey, Password: *standalonePassword,
			ReadPassword: func() (string, error) { return readStandalonePassword(authID) }})
		switch parsedCmd {
		case pingCmd.FullCommand(), connectCmd.FullCommand(), disconnectCmd.FullCommand(), lsCmd.FullCommand(),
			machineLsCmd.FullCommand():
			fmt.Fprintf(os.Stderr, "%s is not available with --url (it works on machines of your account)\n",
				parsedCmd)
			os.Exit(1)
		}
	}

	uri := fmt.Sprintf("unix://%s/deskconn.sock", cfgDirectory)

	switch parsedCmd {
	case keygenCmd.FullCommand():
		publicKey, privateKey, err := wampauth.GenerateCryptoSignKeyPair()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Printf("Public Key:  %s\nPrivate Key: %s\n", publicKey, privateKey)

	case selfVersionCmd.FullCommand():
		fmt.Println(versionString)

	case attachCmd.FullCommand():
		if err := attach(*attachUsername, *attachPassword, *attachName, *attachPasswordStdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case detachCmd.FullCommand():
		if err := detach(cfgDirectory); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case loginCmd.FullCommand():
		if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
			_, _, credErr := common.ReadCredentials(cfgDirectory)
			if errors.Is(credErr, common.ErrKeyExpired) {
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
		session, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}
		callResp := session.Call(common.ProcedureLogin).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case lsFileCmd.FullCommand():
		machine, path := parseMachinePath(*lsFileTarget)
		realm, err := machineRealm(machine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		args := []string{"ls"}
		if path != "" {
			args = append(args, path)
		}
		if err := deskconn.RunExec(context.Background(), *lsFileModeFlag, realm, cfgDirectory, args); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case mvCmd.FullCommand():
		srcMachine, srcPath := parseMachinePath(*mvSrc)
		dstMachine, dstPath := parseMachinePath(*mvDst)
		if srcMachine != dstMachine {
			fmt.Fprintln(os.Stderr, "cross-machine move not supported")
			return
		}
		realm, err := machineRealm(srcMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		payload, _ := json.Marshal(map[string]string{"old_path": srcPath, "new_path": dstPath})
		err = fileOp(context.Background(), uri, realm, cfgDirectory, common.ProcedureFileRename, payload, *mvModeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case cpCmd.FullCommand():
		srcRemote := isRemotePath(*cpSrc)
		dstRemote := isRemotePath(*cpDst)
		switch {
		case srcRemote && dstRemote:
			srcMachine, srcPath := parseMachinePath(*cpSrc)
			dstMachine, dstPath := parseMachinePath(*cpDst)
			if srcMachine != dstMachine {
				fmt.Fprintln(os.Stderr, "cross-machine copy not supported")
				return
			}
			realm, err := machineRealm(srcMachine, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			payload, _ := json.Marshal(map[string]string{"src": srcPath, "dst": dstPath})
			err = fileOp(context.Background(), uri, realm, cfgDirectory, common.ProcedureFileCopy, payload, *cpModeFlag)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case !srcRemote && dstRemote:
			dstMachine, dstPath := parseMachinePath(*cpDst)
			realm, err := machineRealm(dstMachine, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			switch *cpModeFlag {
			case ModeQUIC:
				quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.UploadFilesQUIC(quicSess, realm, *cpSrc, dstPath, *cpRecursive,
					*cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			case ModeP2P:
				p2pSess, err := deskconn.ConnectMachineRealmP2PSession(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Close() }()
				if err := deskconn.UploadFilesP2P(deskconn.P2PChannels(p2pSess), *cpSrc, dstPath,
					*cpRecursive, *cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			default:
				daemon, err := deskconn.DialDaemonStreams(context.Background(), realm, cfgDirectory)
				if !errors.Is(err, deskconn.ErrDaemonUnavailable) {
					if err == nil {
						err = deskconn.UploadFilesDaemon(daemon, *cpSrc, dstPath, *cpRecursive, *cpStreams)
					}
					if err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
					return
				}
				p2pSess, p2pErr := deskconn.ConnectMachineRealmP2PSession(context.Background(), realm, cfgDirectory)
				if p2pErr == nil {
					defer func() { _ = p2pSess.Close() }()
					if err := deskconn.UploadFilesP2P(deskconn.P2PChannels(p2pSess), *cpSrc, dstPath,
						*cpRecursive, *cpStreams); err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
					return
				}
				fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
				quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.UploadFilesQUIC(quicSess, realm, *cpSrc, dstPath, *cpRecursive,
					*cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		case srcRemote && !dstRemote:
			srcMachine, srcPath := parseMachinePath(*cpSrc)
			realm, err := machineRealm(srcMachine, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			switch *cpModeFlag {
			case ModeQUIC:
				quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.DownloadFilesQUIC(quicSess, realm, srcPath, *cpDst, *cpRecursive,
					*cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			case ModeP2P:
				p2pSess, err := deskconn.ConnectMachineRealmP2PSession(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer func() { _ = p2pSess.Close() }()
				if err := deskconn.DownloadFilesP2P(deskconn.P2PChannels(p2pSess), srcPath, *cpDst,
					*cpRecursive, *cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			default:
				daemon, err := deskconn.DialDaemonStreams(context.Background(), realm, cfgDirectory)
				if !errors.Is(err, deskconn.ErrDaemonUnavailable) {
					if err == nil {
						err = deskconn.DownloadFilesDaemon(daemon, srcPath, *cpDst, *cpRecursive, *cpStreams)
					}
					if err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
					return
				}
				p2pSess, p2pErr := deskconn.ConnectMachineRealmP2PSession(context.Background(), realm, cfgDirectory)
				if p2pErr == nil {
					defer func() { _ = p2pSess.Close() }()
					if err := deskconn.DownloadFilesP2P(deskconn.P2PChannels(p2pSess), srcPath, *cpDst, *cpRecursive,
						*cpStreams); err != nil {
						fmt.Fprintln(os.Stderr, err)
					}
					return
				}
				fmt.Fprintln(os.Stderr, "p2p unavailable, falling back to quic")
				quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				defer quicSess.Close()
				if err := deskconn.DownloadFilesQUIC(quicSess, realm, srcPath, *cpDst, *cpRecursive,
					*cpStreams); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		default:
			fmt.Fprintln(os.Stderr, "at least one of src or dst must be a remote path (machine:path)")
		}

	case rmCmd.FullCommand():
		machine, path := parseMachinePath(*rmTarget)
		realm, err := machineRealm(machine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		payload, _ := json.Marshal(map[string]string{jsonFieldPath: path})
		err = fileOp(context.Background(), uri, realm, cfgDirectory, common.ProcedureFileDelete, payload, *rmModeFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}

	case catCmd.FullCommand():
		machine, path := parseMachinePath(*catTarget)
		if path == "" {
			fmt.Fprintln(os.Stderr, "path is required")
			return
		}
		realm, err := machineRealm(machine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		switch *catModeFlag {
		case ModeQUIC:
			quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			if err := deskconn.CatFile(quicSess.Session, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		case ModeP2P:
			p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			if err := deskconn.CatFile(p2pSess, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			if err := deskconn.CatFileViaProxy(localSession, realm, path); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case editCmd.FullCommand():
		machine, path := parseMachinePath(*editTarget)
		if path == "" {
			fmt.Fprintln(os.Stderr, "path is required")
			return
		}
		if !deskconn.IsEditableExtension(path) {
			fmt.Fprintf(os.Stderr, "%s: not a text file, editing is not supported\n", path)
			return
		}
		realm, err := machineRealm(machine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var original []byte
		switch *editModeFlag {
		case ModeQUIC:
			quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
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
			p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
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
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
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
		if err := fileOp(context.Background(), uri, realm, cfgDirectory, common.ProcedureFileEdit,
			payload, *editModeFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println("file updated")

	case shellCmd.FullCommand():
		realm, err := machineRealm(*shellMachineName, cfgDirectory)
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
		defer setupAgentForward(*shellModeFlag, realm, cfgDirectory, agentSock)()

		if len(*shellCommand) > 0 {
			err = deskconn.RunExec(context.Background(), *shellModeFlag, realm, cfgDirectory, *shellCommand)
		} else {
			err = deskconn.RunShell(context.Background(), *shellModeFlag, realm, cfgDirectory)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
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

		realm, err := machineRealm(*portForwardMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("Forwarding 127.0.0.1:%s -> %s:%s\n", localPort, *portForwardMachine, remotePort)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		errCh := make(chan error, 1)
		common.SafeGo(func() {
			errCh <- deskconn.RunPortForward(ctx, *portForwardModeFlag, realm, cfgDirectory, remotePort, localPort)
		})

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

		realm, err := machineRealm(*portReverseMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("Reverse forwarding %s:%s -> 127.0.0.1:%s\n", *portReverseMachine, remotePort, localPort)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		errCh := make(chan error, 1)
		common.SafeGo(func() {
			errCh <- deskconn.RunPortReverse(ctx, *portReverseModeFlag, realm, cfgDirectory, remotePort, localPort)
		})

		select {
		case <-sigCh:
			fmt.Println("\nStopping reverse port forwarding...")
			cancel()
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	case lsCmd.FullCommand(), machineLsCmd.FullCommand():
		session, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}

		machinesCallResp := session.Call(common.ProcedureConnectedMachines).Do()
		if machinesCallResp.Err != nil {
			fmt.Fprintln(os.Stderr, machinesCallResp.Err)
			return
		}
		connectedMachines, ok := machinesCallResp.Args()[0].(map[string]any)
		if !ok {
			fmt.Fprintln(os.Stderr, "expected a map of strings")
			return
		}
		fileExists := true
		machinesFromCfg, err := common.MachinesFromCfg(cfgDirectory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fileExists = false
			} else {
				if !lsRefresh {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				fmt.Println("your config is invalid, refreshing from cloud")
			}
		}
		tableHeader := []string{"ID", "NAME", "ALIAS", "CONNECTED SINCE"}
		if lsDetailed {
			tableHeader = append(tableHeader, "ORGANIZATION", "REALM")
		}
		if !lsRefresh && fileExists {
			table := tablewriter.NewWriter(os.Stdout)
			table.Header(tableHeader)
			for _, d := range machinesFromCfg {
				if d.Name == "" && d.Authid == "" && d.Alias == "" {
					continue
				}
				since := connectedSince(connectedMachines, d.Realm)
				if lsDetailed {
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

		machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.Header(tableHeader)

		cfgMap := make(map[string]common.Machine, len(machinesFromCfg))
		for _, d := range machinesFromCfg {
			cfgMap[d.Authid] = d
		}

		nameCount := make(map[string]int)
		for i, d := range machines {
			count := nameCount[d.Name]
			if count > 0 {
				newName := fmt.Sprintf("%s-%s", d.Name, d.Authid[:6])
				machines[i].Name = newName
			}
			if localMachine, ok := cfgMap[d.Authid]; ok {
				machines[i].Alias = localMachine.Alias
			}
			d = machines[i]
			nameCount[d.Name]++
			since := connectedSince(connectedMachines, d.Realm)
			if lsDetailed {
				_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since, d.Organization.Name, d.Realm})
			} else {
				_ = table.Append([]any{shortID(d.Authid), d.Name, d.Alias, since})
			}
		}

		if err = table.Render(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		if err := deskconn.CacheMachines(cfgDirectory, machines); err != nil {
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

		session, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			log.Fatal(err)
		}

		callResp := session.Call(common.ProcedureLogout).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
		}

	case vpnConnectCmd.FullCommand():
		vpnRealm, err := machineRealm(*vpnConnectMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		vpnCtx, vpnCancel := context.WithCancel(context.Background())
		runVPNConnect(vpnCtx, cfgDirectory, vpnRealm, *vpnConnectMachine)
		vpnCancel()

	case vpnServeCmd.FullCommand():
		vpnServeCtx, vpnServeCancel := context.WithCancel(context.Background())
		runVPNServe(vpnServeCtx, cfgDirectory)
		vpnServeCancel()

	case vpnStopCmd.FullCommand():
		runVPNStop(context.Background(), cfgDirectory)

	case pingCmd.FullCommand():
		realm, err := machineRealm(*pingMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		localSession, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		fmt.Printf("PING %s\n", *pingMachine)

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

			callResp := localSession.Call(common.ProcedureProxyPing).Args(realm).Do()
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
		fmt.Printf("\n--- %s ping statistics ---\n", *pingMachine)
		fmt.Printf("%d packets transmitted, %d received, %d%% packet loss\n", sent, received, loss)
		if received > 0 {
			fmt.Printf("rtt min/avg/max = %d/%d/%d ms\n", minMs, totalMs/int64(received), maxMs)
		}

	case connectCmd.FullCommand():
		realm, err := machineRealm(*connectMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		session, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		callResp := session.Call(common.ProcedureConnect).Args(realm).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		fmt.Printf("connected to %s\n", *connectMachine)

	case disconnectCmd.FullCommand():
		session, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if *disconnectAllFlag {
			callResp := session.Call(common.ProcedureDisconnectAll).Do()
			if callResp.Err != nil {
				fmt.Fprintln(os.Stderr, callResp.Err)
				return
			}
			fmt.Println("disconnected all machines")
			return
		}
		if *disconnectMachine == "" {
			fmt.Fprintln(os.Stderr, "specify a machine or use --all")
			return
		}
		realm, err := machineRealm(*disconnectMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		callResp := session.Call(common.ProcedureDisconnect).Args(realm).Do()
		if callResp.Err != nil {
			fmt.Fprintln(os.Stderr, callResp.Err)
			return
		}
		fmt.Printf("disconnected %s\n", *disconnectMachine)

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

	case aliasCmd.FullCommand():
		if *aliasUnset == (*aliasName != "") {
			fmt.Fprintln(os.Stderr, "specify either an alias name or --unset")
			return
		}
		if err := updateMachineAlias(cfgDirectory, *aliasMachine, *aliasName); err != nil {
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
		realm, err := machineRealm(*infoMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		var rawData []byte
		var callErr error
		switch *infoModeFlag {
		case ModeQUIC:
			quicSess, err := common.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer quicSess.Connection().Close()
			rawData, callErr = deskconn.CallFileOp(quicSess.Session, common.ProcedureMachineInfo, nil)
		case ModeP2P:
			p2pSess, err := deskconn.ConnectMachineRealmP2P(context.Background(), realm, cfgDirectory)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer func() { _ = p2pSess.Leave() }()
			rawData, callErr = deskconn.CallFileOp(p2pSess, common.ProcedureMachineInfo, nil)
		default:
			localSession, err := xconn.ConnectAnonymous(context.Background(), uri, common.LocalRealm)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			callResp := localSession.Call(common.ProcedureProxyMachineInfo).Args(realm).Do()
			callErr = callResp.Err
			if callErr == nil {
				rawData, callErr = callResp.ArgBytes(0)
			}
		}
		if callErr != nil {
			fmt.Fprintln(os.Stderr, callErr)
			return
		}
		var info common.MachineInfo
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
		realm, err := machineRealm(*logsMachine, cfgDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		source := ""
		if logsSource != nil {
			source = *logsSource
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		errCh := make(chan error, 1)
		common.SafeGo(func() {
			errCh <- deskconn.RunLogs(ctx, *logsModeFlag, realm, cfgDirectory, source, *logsFollow, *logsTail, *logsSince)
		})

		select {
		case <-sigCh:
			fmt.Println("\nStopping log stream...")
			cancel()
		case err := <-errCh:
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

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
		dispatchAgentCommand(parsedCmd, agentCmds, cfgDirectory)
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
		fmt.Printf("You're already on version %s of desk (the latest version).\n", version)
		return nil
	}

	fmt.Printf("Found update at %s.\n", updateResp.DownloadURL)

	if err := downloadAndInstallUpdate(updateResp.DownloadURL); err != nil {
		return err
	}

	if err := removeLegacyXlinkService(); err != nil {
		return err
	}

	fmt.Println("Restarting deskconnd service...")
	cmd := exec.Command("systemctl", "--user", "restart", "deskconnd")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to restart deskconnd: %w", err)
	}

	fmt.Printf("Updated desk from version %s to %s.\n", version, updateResp.LatestVersion)
	return nil
}

// removeLegacyXlinkService stops and removes the separate xlink service and binary that
// installs before deskconnd embedded xlink had; left running, it would compete with
// deskconnd for deskconn.sock.
func removeLegacyXlinkService() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	serviceFile := filepath.Join(homeDir, ".config", "systemd", "user", "xlink.service")
	if _, err := os.Stat(serviceFile); err == nil {
		fmt.Println("Removing the old xlink service (now part of deskconnd)...")
		_ = exec.Command("systemctl", "--user", "stop", "xlink").Run()    // nolint: gosec
		_ = exec.Command("systemctl", "--user", "disable", "xlink").Run() // nolint: gosec
		if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove xlink service file: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run() // nolint: gosec
	}

	xlinkBin := filepath.Join(homeDir, ".local", "lib", "exec", "xlink")
	if err := os.Remove(xlinkBin); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove %s: %w", xlinkBin, err)
	}
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
	req.Header.Set("User-Agent", "desk-self-update")

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

// agentForwardReadyTimeout bounds how long "desk shell -A" waits for the remote agent
// forwarding listener to come up before giving up and starting a normal (unforwarded) shell.
const agentForwardReadyTimeout = 5 * time.Second

// setupAgentForward starts SSH-agent forwarding (see deskconn.RunAgentForward) when agentSock
// is non-empty, opening its own raw stream/channel independent of the shell's. It blocks until
// the remote listener is confirmed ready (or setup fails/times out) so the shell can safely
// start right after. Forwarding failures are reported as warnings and never block the shell
// from starting. The returned func stops forwarding and must be called (deferred) once the
// shell exits.
func setupAgentForward(mode, realm, cfgDirectory, agentSock string) func() {
	if agentSock == "" {
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	common.SafeGo(func() {
		_ = deskconn.RunAgentForward(ctx, mode, realm, cfgDirectory, agentSock, ready)
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

// detachIfAttached detaches this machine from the cloud account if it is currently
// attached. It is a no-op otherwise.
func detachIfAttached(cfgDirectory string) error {
	credFile, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(credFile); os.IsNotExist(err) {
		return nil
	}
	machineID, err := os.ReadFile(common.MachineIDPath)
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	session, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()

	fmt.Println("Detaching this machine from your account...")
	if err := deskconn.Detach(session, strings.TrimSpace(string(machineID))); err != nil {
		return err
	}
	fmt.Println("Machine detached.")
	return nil
}

func removeApp(cfgDirectory string, skipConfirm bool) error {
	if !skipConfirm {
		confirmed, err := confirmPrompt("Are you sure you want to remove desk and all configuration, "+
			"credentials and machine list? (y/N): ", false)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("remove cancelled")
			return nil
		}
	}

	if err := detachIfAttached(cfgDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to detach this machine from your account: %v\n", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	if err := removeLegacyXlinkService(); err != nil {
		return err
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
		filepath.Join(binDir, "desk"),
		filepath.Join(binDir, vpndProgName),
		filepath.Join(execDir, "deskconnd"),
		filepath.Join(bashCompDir, "desk"),
		filepath.Join(zshCompDir, "_desk"),
	}
	paths = append(paths, legacyCLIPaths(binDir, bashCompDir, zshCompDir)...)

	fmt.Println("Removing desk binaries and shell completions...")
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

	fmt.Println("Removing desk configuration, credentials and machine list...")
	if err := os.RemoveAll(cfgDirectory); err != nil {
		return fmt.Errorf("failed to remove config directory: %w", err)
	}

	fmt.Println("desk has been removed.")
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

	foundDesk := false
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
		case "desk":
			fmt.Println("Installing desk...")
			deskPath := filepath.Join(binDir, "desk")
			if err := installBinaryFromReader(tarReader, deskPath, 0755); err != nil {
				return err
			}
			// desk runs as the privileged VPN helper when invoked as "vpnd".
			linkPath := filepath.Join(binDir, vpndProgName)
			_ = os.Remove(linkPath)
			if err := os.Symlink(deskPath, linkPath); err != nil {
				return fmt.Errorf("failed to create %s symlink: %w", vpndProgName, err)
			}
			foundDesk = true
		case "deskconnd":
			fmt.Println("Installing deskconnd...")
			if err := installBinaryFromReader(tarReader, filepath.Join(execDir, "deskconnd"), 0700); err != nil {
				return err
			}
			foundDeskconnd = true
		}
	}

	if !foundDesk || !foundDeskconnd {
		return fmt.Errorf("update archive missing required binaries")
	}

	if err := installCompletions(filepath.Join(binDir, "desk")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update shell completions: %v\n", err)
	}

	bashCompDir := filepath.Join(homeDir, ".local", "share", "bash-completion", "completions")
	zshCompDir := filepath.Join(homeDir, ".local", "share", "zsh", "site-functions")
	for _, p := range legacyCLIPaths(binDir, bashCompDir, zshCompDir) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: failed to remove %s: %v\n", p, err)
		}
	}

	return nil
}

func installCompletions(deskBin string) error {
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

	bashOut, err := exec.Command(deskBin, "--completion-script-bash").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating bash completion: %w", err)
	}
	bashScript := fixBashCompletionSpacing(fixBashCompletionWordBreaks(string(bashOut)))
	if err := os.WriteFile(filepath.Join(bashDir, "desk"), []byte(bashScript), 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing bash completion: %w", err)
	}

	zshOut, err := exec.Command(deskBin, "--completion-script-zsh").Output() // nolint: gosec
	if err != nil {
		return fmt.Errorf("generating zsh completion: %w", err)
	}
	if err := os.WriteFile(filepath.Join(zshDir, "_desk"), zshOut, 0644); err != nil { // nolint: gosec
		return fmt.Errorf("writing zsh completion: %w", err)
	}

	return nil
}

// fixBashCompletionWordBreaks clears ':' from COMP_WORDBREAKS so bash
// completes "machine:path" as one word instead of splitting at the colon.
func fixBashCompletionWordBreaks(script string) string {
	const marker = "_desk_bash_autocomplete() {\n"
	return strings.Replace(script, marker, marker+"    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}\n", 1)
}

// fixBashCompletionSpacing makes path completions behave like "cd": the menu shows only the last
// path segment (-o filenames), and no trailing space is added after directories/"machine:" prefixes.
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
	file, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("machine already attached")
	}

	username, password, err := readCredentials(flagUsername, flagPassword, useStdin)
	if err != nil {
		return err
	}

	machineName, err := resolveMachineName(name)
	if err != nil {
		return err
	}

	return deskconn.Attach(username, password, machineName)
}

func resolveMachineName(name string) (string, error) {
	if name != "" {
		return name, nil
	}

	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Name of the new machine [default=%s]: ", host)

	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return host, nil
	}
	return input, nil
}

func promptAttachMachine(username, password string) error {
	file, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return nil
	}

	confirmed, err := confirmPrompt("No machines found on your account. Attach this machine now? (Y/n): ", true)
	if err != nil {
		return err
	}
	if !confirmed {
		return nil
	}

	machineName, err := resolveMachineName("")
	if err != nil {
		return err
	}

	if err := deskconn.Attach(username, password, machineName); err != nil {
		return err
	}

	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		return err
	}

	machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
	if err != nil {
		return fmt.Errorf("failed to fetch machines: %w", err)
	}

	return deskconn.CacheMachines(cfgDirectory, machines)
}

// detach detaches this machine from the user's account after they confirm, and removes
// its credentials so it can be attached again.
func detach(cfgDirectory string) error {
	machineID, err := os.ReadFile(common.MachineIDPath)
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	session, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()

	fmt.Println("This machine will be removed from your account and can't be reached remotely " +
		"until you attach it again.")
	confirmed, err := confirmPrompt("Detach this machine? (y/N): ", false)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("detach cancelled")
		return nil
	}

	if err := deskconn.Detach(session, strings.TrimSpace(string(machineID))); err != nil {
		return err
	}

	// deskconnd also removes it when it hears about the detach, but not if it isn't running,
	// and attach refuses to run while it's there.
	credFile, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(credFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("machine detached, but failed to remove its credentials: %w", err)
	}

	fmt.Println("Machine detached.")
	return nil
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
	common.SafeGo(func() {
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

	machines, err := deskconn.Login(quicSess.Session, username, otp)
	if err != nil {
		return err
	}

	if len(machines) == 0 {
		return promptAttachMachine(username, password)
	}

	return nil
}

func logout(cfgDirectory string) error {
	cloudSession, err := deskconn.ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), deskconn.ErrAuthenticationFailed) ||
			errors.Is(err, common.ErrKeyExpired) {
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

// standaloneRequested reports whether the CLI is pointed at a standalone machine (--url or
// DESKCONN_URL). It has to be known before the command line is parsed: machine arguments
// aren't defined then.
func standaloneRequested() bool {
	if os.Getenv("DESKCONN_URL") != "" {
		return true
	}
	for _, arg := range os.Args[1:] {
		if arg == "--" {
			break
		}
		if arg == "--url" || strings.HasPrefix(arg, "--url=") {
			return true
		}
	}
	return false
}

// machineArg defines cmd's machine argument. With --url there is only one machine, so the
// argument isn't defined at all and the result is empty.
func machineArg(cmd *kingpin.CmdClause, help, cfgDirectory string) *string {
	if standaloneRequested() {
		return new(string)
	}
	return cmd.Arg("machine", help).Required().HintAction(machineCompletions(cfgDirectory)).String()
}

// modeFlag defines cmd's --mode flag. With --url it defaults to a direct connection: the
// local daemon that some commands go through by default doesn't know the standalone machine.
func modeFlag(cmd *kingpin.CmdClause, help string) *string {
	flag := cmd.Flag("mode", help)
	if standaloneRequested() {
		flag = flag.Default(ModeQUIC)
	}
	return flag.Enum(ModeQUIC, ModeP2P)
}

func machineRealm(machineName, cfgDirectory string) (string, error) {
	if standaloneRequested() {
		return common.StandaloneRealm, nil
	}
	machines, err := common.MachinesFromCfg(cfgDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			machines, err := deskconn.FetchMachinesFromCloud(cfgDirectory)
			if err != nil {
				return "", err
			}

			if err := deskconn.CacheMachines(cfgDirectory, machines); err != nil {
				return "", err
			}

			return machineRealm(machineName, cfgDirectory)
		}
		return "", err
	}

	for _, d := range machines {
		if d.Name == machineName {
			return d.Realm, nil
		}
		if d.Authid == machineName {
			return d.Realm, nil
		}
		if d.Alias == machineName {
			return d.Realm, nil
		}
	}

	for _, d := range machines {
		if strings.HasPrefix(d.Authid, machineName) {
			return d.Realm, nil
		}
	}

	return "", fmt.Errorf("machine not found: %s", machineName)
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

// readStandalonePassword prompts for authID's password on the standalone machine, when
// neither --private-key nor --secret was given and a command first connects.
func readStandalonePassword(authID string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) { // #nosec
		return "", errors.New("--url needs --private-key (or DESKCONN_PRIVATE_KEY; see `desk keygen`) " +
			"or --secret (or DESKCONN_SECRET)")
	}

	fmt.Fprintf(os.Stderr, "Password for %s: ", authID)
	pwd, err := term.ReadPassword(int(os.Stdin.Fd())) // #nosec
	fmt.Fprint(os.Stderr, "\r\n")
	if err != nil {
		return "", err
	}

	return string(pwd), nil
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

func machineCompletions(cfgDirectory string) func() []string {
	return func() []string {
		machines, err := common.MachinesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range machines {
			if d.Name != "" {
				names = append(names, d.Name)
			}
		}
		return names
	}
}

func machinePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		machines, err := common.MachinesFromCfg(cfgDirectory)
		if err != nil {
			return nil
		}
		var names []string
		for _, d := range machines {
			if d.Name != "" {
				names = append(names, d.Name+":")
			}
		}
		return names
	}
}

// remotePathCompletions falls back to local filename completion when no
// "machine:" prefix is typed, and browses the remote directory once one is.
func remotePathCompletions(cfgDirectory string) func() []string {
	return func() []string {
		current := currentCompletionArg()
		if !isRemotePath(current) {
			return machinePathCompletions(cfgDirectory)()
		}
		return remoteMachinePathCompletions(cfgDirectory, current)
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

// remoteMachinePathCompletions lists entries of the directory being typed in current.
func remoteMachinePathCompletions(cfgDirectory, current string) []string {
	machine, path := parseMachinePath(current)
	realm, err := machineRealm(machine, cfgDirectory)
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
	localSession, err := xconn.ConnectAnonymous(ctx, uri, common.LocalRealm)
	if err != nil {
		return nil
	}
	defer func() { _ = localSession.Leave() }()

	if !machineHasPersistentSession(localSession, realm) {
		_ = localSession.Call(common.ProcedureConnect).Args(realm).Do()
		return nil
	}

	browsePayload, err := json.Marshal(map[string]string{jsonFieldPath: dir})
	if err != nil {
		return nil
	}
	resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, common.ProcedureFileBrowse, browsePayload).Do()
	if resp.Err != nil {
		return nil
	}
	result, err := resp.ArgBytes(0)
	if err != nil {
		return nil
	}

	var browsed common.FileBrowseResult
	if err := json.Unmarshal(result, &browsed); err != nil {
		return nil
	}

	var out []string
	for _, entry := range browsed.Entries {
		if !strings.HasPrefix(entry.Name, prefix) {
			continue
		}
		completion := machine + ":" + dir + entry.Name
		if entry.IsDir {
			completion += "/"
		}
		out = append(out, completion)
	}
	return out
}

func machineHasPersistentSession(localSession *xconn.Session, realm string) bool {
	resp := localSession.Call(common.ProcedureConnectedMachines).Do()
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

func parseMachinePath(s string) (machine, path string) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

func fileOp(ctx context.Context, uri, realm, cfgDirectory, procedure string, payload []byte, mode string) error {
	switch mode {
	case ModeQUIC:
		quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer quicSess.Connection().Close()
		_, err = deskconn.CallFileOp(quicSess.Session, procedure, payload)
		return err
	case ModeP2P:
		p2pSess, err := deskconn.ConnectMachineRealmP2P(ctx, realm, cfgDirectory)
		if err != nil {
			return err
		}
		defer func() { _ = p2pSess.Leave() }()
		_, err = deskconn.CallFileOp(p2pSess, procedure, payload)
		return err
	default:
		localSession, err := xconn.ConnectAnonymous(ctx, uri, common.LocalRealm)
		if err != nil {
			return err
		}
		resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, procedure, payload).Do()
		return resp.Err
	}
}

func shortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[:6]
}

func connectedSince(connectedMachines map[string]any, realm string) string {
	v, ok := connectedMachines[realm]
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

func updateMachineAlias(cfgDirectory, machineKey, alias string) error {
	configFile := filepath.Join(cfgDirectory, "config.yml")

	data, err := os.ReadFile(configFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no config found. User not logged in")
		}
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config common.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	machines := config.Machines
	for _, d := range machines {
		if (d.Alias == alias && d.Alias != "") || d.Authid == alias || d.Name == alias {
			return fmt.Errorf("alias '%s' already in use", alias)
		}
	}

	updated := false
	for i, d := range machines {
		if d.Authid == machineKey || d.Name == machineKey || d.Alias == machineKey {
			machines[i].Alias = alias
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("machine %s not found", machineKey)
	}

	out, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(configFile, out, 0600)
}
