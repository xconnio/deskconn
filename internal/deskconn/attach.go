package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

// Attach adds this machine to username's machines as desktopName and keeps the key its
// daemon serves it with.
func Attach(username, password, desktopName string) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}

	machineIDStr, err := xconnd.MachineID()
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}

	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate cryptosign keypair: %w", err)
	}

	realm, err := c.Attach(context.Background(), username, password, machineIDStr, publicKey, desktopName)
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
