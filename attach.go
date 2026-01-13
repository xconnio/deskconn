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
	ProcedureDeskconnOrganizationList = "io.xconn.deskconn.organization.list"
	MachineIDPath                     = "/etc/machine-id"
)

func CloudURI() string {
	if v, ok := os.LookupEnv("DESKCONN_CLOUD_URI"); ok {
		return v
	}
	return "ws://159.65.112.187:8080/ws"
}

type Credentials struct {
	AuthID         string `json:"auth_id"`
	PublicKey      string `json:"public_key"`
	PrivateKey     string `json:"private_key"`
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

	callResp := session.Call(ProcedureDeskconnAttachDesktop).Args(machineIDStr, publicKey, orgID).
		Kwarg("name", desktopName).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to attach desktop: %w", callResp.Err)
	}

	return writeCredentialsFile(machineIDStr, publicKey, privateKey, orgID)
}

func writeCredentialsFile(machineID, publicKey, privateKey, orgID string) error {
	credFilePath, err := credentialsFilePath()
	if err != nil {
		return err
	}

	creds := Credentials{
		AuthID:         machineID,
		PublicKey:      publicKey,
		PrivateKey:     privateKey,
		OrganizationID: orgID,
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	return os.WriteFile(credFilePath, data, 0600)
}
