package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

func Attach(username, password, desktopName string) error {
	quicSess, err := ConnectCloudCRA(context.Background(), username, password)
	if err != nil {
		return err
	}
	common.SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	defer quicSess.Connection().Close()
	session := quicSess.Session

	machineID, err := os.ReadFile(common.MachineIDPath)
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}
	machineIDStr := strings.TrimSpace(string(machineID))

	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate cryptosign keypair: %w", err)
	}

	callResp := session.Call(common.ProcedureDeskconnAttachDesktop).Args(machineIDStr, publicKey, desktopName).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to attach desktop: %w", callResp.Err)
	}

	respDict, err := callResp.ArgDict(0)
	if err != nil {
		return err
	}

	id, err := respDict.String("realm")
	if err != nil {
		return err
	}

	return writeCredentialsFile(id, machineIDStr, publicKey, privateKey)
}

func Detach(session *xconn.Session, authID string) error {
	callResp := session.Call(common.ProcedureDeskconnDetachDesktop).Args(authID).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to detach desktop: %w", callResp.Err)
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
