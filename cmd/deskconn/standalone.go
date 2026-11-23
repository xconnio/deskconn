package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// then saves it, without connecting: the machine may well be offline.
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
	fmt.Printf("Removed standalone machine %q.\n", name)
	return nil
}

// useStandaloneMachine makes machine's realm resolve to it for this process, asking for its
// password on first connecting if it has no private key, and returns that realm.
func useStandaloneMachine(machine common.StandaloneMachine) string {
	realm := common.StandaloneMachineRealm(machine.Name)
	if _, ok := common.StandaloneTargetFor(realm); ok {
		return realm
	}
	username := machine.User()
	common.SetStandaloneTargetFor(realm, &common.StandaloneTarget{URL: machine.URL, CertHash: machine.CertHash,
		AuthID: username, PrivateKey: machine.PrivateKey,
		ReadPassword: func() (string, error) { return readStandalonePassword(machine.Name, username) }})
	return realm
}

// isStandaloneRealm reports whether realm is a standalone machine's, which this process
// connects to itself: the local daemon only knows the account's machines.
func isStandaloneRealm(realm string) bool {
	_, ok := common.StandaloneTargetFor(realm)
	return ok
}

// defaultMode is the --mode to reach realm's machine with: mode if given, otherwise a routed
// connection of its own for a standalone machine, which the daemon (the default) can't reach.
func defaultMode(realm, mode string) string {
	if mode == "" && isStandaloneRealm(realm) {
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
