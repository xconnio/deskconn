package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	auth "github.com/xconnio/xconn/bindings/go/auth"
)

const (
	modeManaged    = "managed"
	modeStandalone = "standalone"
)

func printKeyPair() error {
	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return err
	}
	fmt.Printf("Public Key:  %s\nPrivate Key: %s\n", publicKey, privateKey)
	return nil
}

// addStandaloneMachine checks machine's name, URL, certificate fingerprint and private key,
// asks for its password if it has no private key, then saves it, without connecting: the
// machine may well be offline. Saving one of them is what lets connecting never ask.
func addStandaloneMachine(cfgDirectory string, machine common.StandaloneMachine) error {
	if strings.TrimSpace(machine.Name) == "" || strings.ContainsAny(machine.Name, " \t\n:") {
		return &usageError{msg: "machine name can't be empty or contain spaces or ':'"}
	}
	if _, err := common.ParseStandaloneURL(machine.URL); err != nil {
		return err
	}
	if _, err := common.StandaloneTLSConfig(machine.CertHash); err != nil {
		return err
	}
	if machine.Username == "" {
		machine.Username = common.CurrentUsername()
	}
	if machine.PrivateKey != "" {
		if _, err := auth.NewCryptoSignAuthenticator(machine.Username, machine.PrivateKey, nil); err != nil {
			return errors.New("invalid private key: expected the hex private key `desk keygen` prints")
		}
	} else {
		password, err := readStandalonePassword(machine.Name, machine.Username)
		if err != nil {
			return err
		}
		machine.Password = password
	}

	if err := deskconn.AddStandaloneMachine(cfgDirectory, machine); err != nil {
		return err
	}
	fmt.Printf("Added standalone machine %q. Connect with desk shell %s\n", machine.Name, machine.Name)
	return nil
}

// removeStandaloneMachine deletes the standalone machine saved as name. Machines of the
// account aren't saved on this client, so there's nothing to remove for them.
func removeStandaloneMachine(cfgDirectory, name string) error {
	err := deskconn.RemoveStandaloneMachine(cfgDirectory, name)
	if errors.Is(err, deskconn.ErrStandaloneMachineNotFound) {
		managed, _ := common.MachinesFromCfg(cfgDirectory)
		if _, ok := deskconn.ManagedMachineNamed(managed, name); ok {
			return fmt.Errorf("%q is a managed machine; desk machine remove only removes standalone machines "+
				"(to remove a managed machine from your account, run desk machine detach on it)", name)
		}
		return fmt.Errorf("no standalone machine named %q; see desk machine ls", name)
	}
	if err != nil {
		return err
	}
	disconnectRemovedMachine(cfgDirectory, name)
	fmt.Printf("Removed standalone machine %q.\n", name)
	return nil
}

// disconnectRemovedMachine has the local daemon, if it's running, drop its connection to the
// standalone machine just removed as name: one added again under that name, maybe with
// another URL or login, gets a connection of its own rather than the old one.
func disconnectRemovedMachine(cfgDirectory, name string) {
	if !daemonRunning(cfgDirectory) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := common.ConnectDaemon(ctx, cfgDirectory)
	if err != nil {
		return
	}
	defer func() { _ = session.Leave() }()
	kwargs := map[string]any{common.DisconnectKwargRealm: common.StandaloneMachineRealm(name)}
	if err := session.Call(common.ProcedureDisconnect).Kwargs(kwargs).ExecuteContext(ctx).Err; err != nil {
		fmt.Fprintf(os.Stderr, "warning: the local daemon may still be connected to %s: %v\n", name, err)
	}
}

// useStandaloneMachine makes machine's realm resolve to it for this process, and returns
// that realm.
func useStandaloneMachine(machine common.StandaloneMachine) string {
	realm := common.StandaloneMachineRealm(machine.Name)
	if _, ok := common.StandaloneTargetFor(realm); !ok {
		common.SetStandaloneTargetFor(realm, machine.Target())
	}
	return realm
}

// throughDaemon reports whether realm's machine is reached through the local daemon's
// persistent connection by default: a managed machine's always, a standalone machine's while
// the daemon is running. Without the daemon, a standalone machine is reached on a connection
// of its own, as it was before the daemon could keep one: it needs neither daemon nor cloud.
func throughDaemon(cfgDirectory, realm string) bool {
	return !common.IsStandaloneMachineRealm(realm) || daemonRunning(cfgDirectory)
}

// defaultMode is the --mode to reach realm's machine with: mode if given, otherwise "" (the
// local daemon's persistent connection), or routed (a connection of its own) for a standalone
// machine while the daemon isn't running (see throughDaemon).
func defaultMode(cfgDirectory, realm, mode string) string {
	if mode == "" && !throughDaemon(cfgDirectory, realm) {
		return ModeRouted
	}
	return mode
}

// ambiguousMachineError reports name referring to both a standalone machine and managed, a
// machine of the account, and how to tell them apart.
func ambiguousMachineError(name string, managed common.Machine) error {
	return fmt.Errorf("%q refers to both a standalone machine and a managed machine of your account (%s); "+
		"use the managed machine's full ID (%s) to reach it, or rename the standalone machine: "+
		"desk machine remove %s, then desk machine add <new-name> --url <url>",
		name, managed.Name, managed.Authid, name)
}

// loggedIn reports whether the CLI has an account login, whose machines it can fetch.
func loggedIn(cfgDirectory string) bool {
	_, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519"))
	return err == nil
}

// daemonRunning reports whether the current user's local daemon is listening on its socket:
// a socket left behind by one that's gone refuses the connection.
func daemonRunning(cfgDirectory string) bool {
	conn, err := net.DialTimeout("unix", filepath.Join(cfgDirectory, "deskconn.sock"), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
