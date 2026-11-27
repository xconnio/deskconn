package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

// Attach adds this machine to the user's machines as desktopName, signing in for it the way
// Login does: the user approves a key in the browser, handed where by prompt. That key is
// only for this attach and is revoked afterwards; the CLI's own login is neither used nor
// changed. Failing to revoke it only warns: the machine is attached, and the key stays
// valid until it expires, a month later.
func Attach(ctx context.Context, desktopName string, prompt func(url, userCode string)) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}

	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	authid, expiresAt, err := c.Login(ctx, publicKey, loginHostname(), attachPurpose, prompt)
	if err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}

	session, err := ConnectCloudCryptosign(ctx, authid, privateKey)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()
	defer func() {
		if err := c.Logout(session, publicKey); err != nil {
			until := "it expires"
			if !expiresAt.IsZero() {
				until = expiresAt.Local().Format(time.DateTime)
			}
			log.Warnf("failed to revoke the key attach signed in with: it stays valid on your account "+
				"until %s, unless you remove it there: %v", until, err)
		}
	}()

	return attachMachine(c, session, desktopName)
}

// AttachSignedIn adds this machine as desktopName with the CLI's own login, as offered right
// after logging in.
func AttachSignedIn(cfgDirectory, desktopName string) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}

	session, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		return err
	}
	defer func() { _ = session.Leave() }()

	return attachMachine(c, session, desktopName)
}

// attachMachine adds this machine on session's account and keeps the key its daemon serves
// it with.
func attachMachine(c *Cloud, session *xconn.Session, desktopName string) error {
	machineIDStr, err := xconnd.MachineID()
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate cryptosign keypair: %w", err)
	}

	realm, err := c.Attach(session, machineIDStr, publicKey, desktopName)
	if err != nil {
		return fmt.Errorf("failed to attach desktop: %w", err)
	}

	return writeCredentialsFile(realm, machineIDStr, publicKey, privateKey)
}

func Detach(session *xconn.Session, authID string) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}
	if err := c.Detach(session, authID); err != nil {
		return fmt.Errorf("failed to detach desktop: %w", err)
	}

	return nil
}

func writeCredentialsFile(realm, machineID, publicKey, privateKey string) error {
	credFilePath, err := common.CredentialsFilePath()
	if err != nil {
		return err
	}

	creds := common.Credentials{
		Realm:      realm,
		AuthID:     machineID,
		PublicKey:  publicKey,
		PrivateKey: privateKey,
	}

	data, err := json.MarshalIndent(creds, "", "  ") // #nosec G117
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	data = append(data, '\n')
	return os.WriteFile(credFilePath, data, 0600)
}
