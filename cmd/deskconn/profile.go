package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/olekukonko/tablewriter"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	wampauth "github.com/xconnio/wampproto-go/auth"
	xconnauth "github.com/xconnio/xconn-go/auth"
)

type profileCommands struct {
	keygen *kingpin.CmdClause
	add    *kingpin.CmdClause
	remove *kingpin.CmdClause
	ls     *kingpin.CmdClause

	addName     *string
	addURL      *string
	addCertHash *string
	addKey      *string
	addAuthID   *string

	removeName *string
}

func registerProfileCommands(app *kingpin.Application, cfgDirectory string) *profileCommands {
	keygenCmd := app.Command("keygen", "Generate a key pair for standalone machines")

	profileCmd := app.Command("profile", "Save standalone machines to connect to with --profile")
	addCmd := profileCmd.Command("add", "Save a standalone machine as a profile")
	addName := addCmd.Arg("name", "Profile name").Required().String()
	addURL := addCmd.Flag("url", "The machine's URL: quic://host:port, or https://host:port "+
		"for WebTransport").Required().String()
	addCertHash := addCmd.Flag("cert-hash", "SHA-256 fingerprint of the machine's certificate, "+
		"as deskconn-userd prints it, to trust it even if self-signed (default: verify against trusted CAs)").String()
	addKey := addCmd.Flag("private-key", "Private key (hex) to authenticate with; see `desk keygen`. "+
		"Without it, a password is asked for on each use").String()
	addAuthID := addCmd.Flag("authid", "Authid (username) to present (default: current user)").String()
	removeCmd := profileCmd.Command("remove", "Remove a profile")
	removeName := removeCmd.Arg("name", "Profile name").Required().
		HintAction(profileCompletions(cfgDirectory)).String()
	lsCmd := profileCmd.Command("ls", "List profiles")

	return &profileCommands{
		keygen:      keygenCmd,
		add:         addCmd,
		remove:      removeCmd,
		ls:          lsCmd,
		addName:     addName,
		addURL:      addURL,
		addCertHash: addCertHash,
		addKey:      addKey,
		addAuthID:   addAuthID,
		removeName:  removeName,
	}
}

func dispatchProfileCommand(parsedCmd string, cmds *profileCommands, cfgDirectory string) bool {
	var err error
	switch parsedCmd {
	case cmds.keygen.FullCommand():
		printErr(printKeyPair())
		return true
	case cmds.add.FullCommand():
		err = addProfile(cfgDirectory, common.Profile{Name: *cmds.addName, URL: *cmds.addURL,
			CertHash: *cmds.addCertHash, PrivateKey: *cmds.addKey, AuthID: *cmds.addAuthID})
	case cmds.remove.FullCommand():
		err = removeProfile(cfgDirectory, *cmds.removeName)
	case cmds.ls.FullCommand():
		err = listProfiles(cfgDirectory)
	default:
		return false
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return true
}

func printKeyPair() error {
	publicKey, privateKey, err := wampauth.GenerateCryptoSignKeyPair()
	if err != nil {
		return err
	}
	fmt.Printf("Public Key:  %s\nPrivate Key: %s\n", publicKey, privateKey)
	return nil
}

// addProfile checks profile's URL, certificate fingerprint and private key, then saves it.
func addProfile(cfgDirectory string, profile common.Profile) error {
	if strings.TrimSpace(profile.Name) == "" || strings.ContainsAny(profile.Name, " \t\n") {
		return fmt.Errorf("profile name can't be empty or contain spaces")
	}
	if _, err := common.ParseStandaloneURL(profile.URL); err != nil {
		return err
	}
	if _, err := common.StandaloneTLSConfig(profile.CertHash); err != nil {
		return err
	}
	if profile.AuthID == "" {
		profile.AuthID = currentUsername()
	}
	if profile.PrivateKey != "" {
		if _, err := xconnauth.NewCryptoSignAuthenticator(profile.AuthID, profile.PrivateKey, nil); err != nil {
			return errors.New("invalid private key: expected the hex private key `desk keygen` prints")
		}
	}

	if err := deskconn.AddProfile(cfgDirectory, profile); err != nil {
		return err
	}
	fmt.Printf("Saved profile %s. Use it with: desk --profile %s <command>\n", profile.Name, profile.Name)
	return nil
}

func removeProfile(cfgDirectory, name string) error {
	if err := deskconn.RemoveProfile(cfgDirectory, name); err != nil {
		return err
	}
	fmt.Printf("Removed profile %s.\n", name)
	return nil
}

// useProfile points this process at the standalone machine saved as name.
func useProfile(cfgDirectory, name string) error {
	profile, err := deskconn.FindProfile(cfgDirectory, name)
	if errors.Is(err, deskconn.ErrProfileNotFound) {
		return fmt.Errorf("no profile named %s; see `desk profile ls`", name)
	}
	if err != nil {
		return err
	}
	authID := profile.AuthID
	if authID == "" {
		authID = currentUsername()
	}
	common.SetStandaloneTarget(&common.StandaloneTarget{URL: profile.URL, CertHash: profile.CertHash,
		AuthID: authID, PrivateKey: profile.PrivateKey,
		ReadPassword: func() (string, error) { return readStandalonePassword(authID) }})
	return nil
}

func listProfiles(cfgDirectory string) error {
	profiles, err := deskconn.Profiles(cfgDirectory)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		fmt.Println("No profiles. Add one with `desk profile add <name> --url <url>`.")
		return nil
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.Header([]string{"Name", "URL", "Auth", "Certificate"})
	for _, profile := range profiles {
		auth := "password as " + profile.AuthID
		if profile.PrivateKey != "" {
			auth = "key as " + profile.AuthID
		}
		cert := "trusted CAs"
		if profile.CertHash != "" {
			cert = "pinned"
		}
		_ = table.Append([]any{profile.Name, profile.URL, auth, cert})
	}
	return table.Render()
}

// currentUsername is the authid a profile presents unless --authid says otherwise.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "deskconn"
}

func profileCompletions(cfgDirectory string) func() []string {
	return func() []string {
		profiles, _ := deskconn.Profiles(cfgDirectory)
		names := make([]string, 0, len(profiles))
		for _, profile := range profiles {
			names = append(names, profile.Name)
		}
		return names
	}
}
