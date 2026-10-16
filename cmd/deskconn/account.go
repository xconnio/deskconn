package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/xconn-go"
)

type accountCommands struct {
	login  *kingpin.CmdClause
	whoami *kingpin.CmdClause
	logout *kingpin.CmdClause

	loginUsername *string
}

func registerAccountCommands(app *kingpin.Application) *accountCommands {
	accountCmd := app.Command("account", "Manage CLI authentication")

	loginCmd := accountCmd.Command("login", "Sign in to access your machines")
	loginUsername := loginCmd.Flag("username", "Username").Short('u').String()
	whoamiCmd := accountCmd.Command("whoami", "Show your account")
	logoutCmd := accountCmd.Command("logout", "Sign out of the CLI")

	return &accountCommands{
		login:         loginCmd,
		whoami:        whoamiCmd,
		logout:        logoutCmd,
		loginUsername: loginUsername,
	}
}

func dispatchAccountCommand(parsedCmd string, cmds *accountCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.login.FullCommand():
		printErr(runLogin(cfgDirectory, *cmds.loginUsername))
	case cmds.whoami.FullCommand():
		printErr(whoami(cfgDirectory))
	case cmds.logout.FullCommand():
		printErr(runLogout(cfgDirectory))
	default:
		return false
	}
	return true
}

// runLogin signs in, unless already signed in with a key that hasn't expired, and tells
// the local daemon.
func runLogin(cfgDirectory, username string) error {
	if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
		_, _, credErr := common.ReadCredentials(cfgDirectory)
		if !errors.Is(credErr, common.ErrKeyExpired) {
			return errors.New("you are already logged in, please logout first")
		}
		_ = deskconn.RemoveCredentialsFiles(cfgDirectory)
	}

	if err := login(username); err != nil {
		return err
	}
	session, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}
	return session.Call(common.ProcedureLogin).Do().Err
}

// runLogout signs out and tells the local daemon.
func runLogout(cfgDirectory string) error {
	if err := logout(cfgDirectory); err != nil {
		return err
	}
	session, err := xconn.ConnectAnonymous(context.Background(), daemonURI(cfgDirectory), common.LocalRealm)
	if err != nil {
		log.Fatal(err)
	}
	return session.Call(common.ProcedureLogout).Do().Err
}

func whoami(cfgDirectory string) error {
	credentialsStr, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519.pub"))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("user not logged in")
	}
	if err != nil {
		return err
	}
	credentials := strings.Split(strings.TrimSpace(string(credentialsStr)), " ")

	if len(credentials) > 2 {
		fmt.Printf("Logged in as %s (%s).\n", credentials[2], credentials[1])
	} else {
		fmt.Printf("Logged in as %s.\n", credentials[1])
	}
	return nil
}

func login(flagUsername string) error {
	username, password, err := readCredentials(flagUsername)
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
