package deskconn

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const (
	Realm                             = "io.xconn.deskconn"
	ProcedureDeskconnAttachDesktop    = "io.xconn.deskconn.desktop.attach"
	ProcedureDeskconnDetachDesktop    = "io.xconn.deskconn.desktop.detach"
	ProcedureDeskconnOrganizationList = "io.xconn.deskconn.organization.list"
	ProcedureOrganizationCreate       = "io.xconn.deskconn.organization.create"
	TopicDeskconnDesktopDetachFormat  = "io.xconn.deskconn.desktop.%s.detach"
	MachineIDPath                     = "/etc/machine-id"
)

func CloudURI() string {
	if v, ok := os.LookupEnv("DESKCONN_CLOUD_URI"); ok {
		return v
	}
	return "wss://api.deskconn.com/ws"
}

type Credentials struct {
	AuthID         string `json:"authid"`
	PublicKey      string `json:"public_key"`
	PrivateKey     string `json:"private_key"` // #nosec
	OrganizationID string `json:"organization_id"`
}

func Attach(session *xconn.Session, desktopName, orgID string) error {
	machineID, err := os.ReadFile(MachineIDPath)
	if err != nil {
		return fmt.Errorf("failed to read machine-id: %w", err)
	}
	machineIDStr := strings.TrimSpace(string(machineID))

	publicKey, privateKey, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate cryptosign keypair: %w", err)
	}

	callResp := session.Call(ProcedureDeskconnAttachDesktop).Args(machineIDStr, publicKey, orgID, desktopName).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to attach desktop: %w", callResp.Err)
	}

	return writeCredentialsFile(machineIDStr, publicKey, privateKey, orgID)
}

func Detach(session *xconn.Session, authID string) error {
	callResp := session.Call(ProcedureDeskconnDetachDesktop).Args(authID).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to detach desktop: %w", callResp.Err)
	}

	return nil
}

func writeCredentialsFile(machineID, publicKey, privateKey, orgID string) error {
	credFilePath, err := CredentialsFilePath()
	if err != nil {
		return err
	}

	creds := Credentials{
		AuthID:         machineID,
		PublicKey:      publicKey,
		PrivateKey:     privateKey,
		OrganizationID: orgID,
	}

	data, err := json.MarshalIndent(creds, "", "  ") // #nosec G117
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	data = append(data, '\n')
	return os.WriteFile(credFilePath, data, 0600)
}
