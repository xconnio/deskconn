package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
)

type authCommands struct {
	login  *kingpin.CmdClause
	whoami *kingpin.CmdClause
	logout *kingpin.CmdClause

	loginNoBrowser *bool
}

func registerAuthCommands(app *kingpin.Application) *authCommands {
	authCmd := app.Command("auth", "Manage CLI authentication")

	loginCmd := authCmd.Command("login", "Sign in to access your machines")
	loginNoBrowser := loginCmd.Flag("no-browser", "Print the login URL instead of opening a browser").Bool()
	whoamiCmd := authCmd.Command("whoami", "Show your account")
	logoutCmd := authCmd.Command("logout", "Sign out of the CLI")

	return &authCommands{
		login:          loginCmd,
		whoami:         whoamiCmd,
		logout:         logoutCmd,
		loginNoBrowser: loginNoBrowser,
	}
}

func dispatchAuthCommand(parsedCmd string, cmds *authCommands, cfgDirectory string) bool {
	switch parsedCmd {
	case cmds.login.FullCommand():
		exitOnErr(runLogin(cfgDirectory, *cmds.loginNoBrowser))
	case cmds.whoami.FullCommand():
		exitOnErr(whoami(cfgDirectory))
	case cmds.logout.FullCommand():
		exitOnErr(runLogout(cfgDirectory))
	default:
		return false
	}
	return true
}

// runLogin signs in, unless already signed in with a key that hasn't expired, and tells
// the local daemon.
func runLogin(cfgDirectory string, noBrowser bool) error {
	if _, err := os.Stat(filepath.Join(cfgDirectory, "id_ed25519")); err == nil {
		_, _, credErr := common.ReadCredentials(cfgDirectory)
		if !errors.Is(credErr, common.ErrKeyExpired) {
			return errors.New("you are already logged in, please logout first")
		}
		_ = deskconn.RemoveCredentialsFiles(cfgDirectory)
	}

	if err := login(cfgDirectory, noBrowser); err != nil {
		return err
	}
	session, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		log.Fatal(err)
	}
	return session.Call(common.ProcedureLogin).Execute().Err
}

// runLogout signs out and tells the local daemon.
func runLogout(cfgDirectory string) error {
	if err := deskconn.Logout(cfgDirectory); err != nil {
		return err
	}
	session, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		log.Fatal(err)
	}
	return session.Call(common.ProcedureLogout).Execute().Err
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

func login(cfgDirectory string, noBrowser bool) error {
	// Only while waiting for approval: Ctrl-C at the attach prompt after it must still exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	machines, err := deskconn.Login(ctx, func(url, userCode string) {
		fmt.Fprintf(os.Stderr, "To sign in, open this page on any device and enter the code:\n\n  %s\n\n  Code: %s\n\n",
			url, userCode)
		if !noBrowser {
			openBrowser(url)
		}
		fmt.Fprintln(os.Stderr, "Waiting for approval...")
	})
	stop()
	if err != nil {
		return err
	}

	if len(machines) == 0 {
		return promptAttachMachine(cfgDirectory)
	}

	return nil
}

// openBrowser opens url in the default browser, unless this is an SSH session or has no
// display, where the URL printed beside it is all there is.
func openBrowser(url string) {
	if os.Getenv("SSH_CONNECTION") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case goosDarwin:
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		common.SafeGo(func() { _ = cmd.Wait() })
	}
}
