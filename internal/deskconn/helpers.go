package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
	xconnauth "github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	ProcedurePrincipalDelete    = "io.xconn.deskconn.account.principal.delete"
	ProcedureAccountGet         = "io.xconn.deskconn.account.get"
	ProcedureAccountLogin       = "io.xconn.deskconn.account.login"
	ProcedureAccountLoginVerify = "io.xconn.deskconn.account.login.verify"

	ProcedureListDesktop = "io.xconn.deskconn.desktop.list"

	ErrAuthenticationFailed = "wamp.error.authentication_failed"
)

func Login(session *xconn.Session, username, otp string) ([]common.Machine, error) {
	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		return nil, err
	}

	privPath := filepath.Join(cfgDirectory, "id_ed25519")
	pubPath := filepath.Join(cfgDirectory, "id_ed25519.pub")

	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate keypair: %w", err)
	}

	callResp := session.Call(ProcedureAccountLoginVerify).Args(username, otp, pub).Do()
	if callResp.Err != nil {
		return nil, fmt.Errorf("failed to verify otp: %w", callResp.Err)
	}

	principal, err := callResp.ArgDict(0)
	if err != nil {
		return nil, fmt.Errorf("unexpected response from login verify: %w", err)
	}
	expiresAtStr, err := principal.String("expires_at")
	if err != nil {
		return nil, fmt.Errorf("missing expires_at in login verify response: %w", err)
	}

	cloudSession, err := ConnectCloudCryptosign(username, priv)
	if err != nil {
		return nil, err
	}

	accountGetResp := cloudSession.Call(ProcedureAccountGet).Do()
	if accountGetResp.Err != nil {
		return nil, fmt.Errorf("failed to get account: %w", accountGetResp.Err)
	}
	account, err := accountGetResp.ArgDict(0)
	if err != nil {
		return nil, fmt.Errorf("unexpected response from account get: %w", err)
	}
	name, err := account.String("name")
	if err != nil {
		return nil, fmt.Errorf("missing name in account get response: %w", err)
	}
	if err = os.WriteFile(privPath, []byte(priv+" "+username+" "+expiresAtStr+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	if err = os.WriteFile(pubPath, []byte(pub+" "+username+" "+name+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	machines, err := fetchMachines(cloudSession)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch machines: %w", err)
	}

	if err := CacheMachines(cfgDirectory, machines); err != nil {
		return nil, err
	}

	return machines, nil
}

func ConnectCloudRealm(cfgDirectory string) (*xconn.Session, error) {
	authid, privKey, err := common.ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	return ConnectCloudCryptosign(authid, privKey)
}

func ConnectCloudCryptosign(authid, privKey string) (*xconn.Session, error) {
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticator: %w", err)
	}

	quicSess, err := xconn.ConnectQUIC(context.Background(), common.CloudQUICAddress(), common.Realm,
		&xconn.QUICDialerConfig{
			Authenticator: authenticator,
			TLSConfig:     common.CloudQUICTLSConfig(),
		})
	if err != nil {
		return nil, err
	}

	common.SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	return quicSess.Session, nil
}

func ConnectCloudCRA(ctx context.Context, username, password string) (*xconn.QUICSession, error) {
	authenticator := xconnauth.NewWAMPCRAAuthenticator(username, password, nil)
	return xconn.ConnectQUIC(ctx, common.CloudQUICAddress(), common.Realm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     common.CloudQUICTLSConfig(),
	})
}

func RemoveCredentialsFiles(cfgDirectory string) error {
	files := []string{
		filepath.Join(cfgDirectory, "id_ed25519"),
		filepath.Join(cfgDirectory, "id_ed25519.pub"),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	// Drop the cloud machines but keep direct machines and other settings.
	cfgPath := filepath.Join(cfgDirectory, "config.yml")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := CacheMachines(cfgDirectory, nil); err != nil {
		return os.Remove(cfgPath) // unreadable config: drop it, as before
	}
	return nil
}

// ConnectMachineRealmP2P connects directly to a machine via WebRTC P2P, using QUIC for signaling.
func ConnectMachineRealmP2P(ctx context.Context, realm, cfgDirectory string) (*xconn.Session, error) {
	webrtcSess, err := ConnectMachineRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

// ConnectMachineRealmP2PSession is like ConnectMachineRealmP2P but returns the
// full WebRTC session instead of just the WAMP session on top of it, giving
// access to the shared PeerConnection: OpenChannel (for raw, non-WAMP data
// channels such as the IP tunnel) and Connection (e.g. to inspect the
// selected ICE candidate pair).
func ConnectMachineRealmP2PSession(
	ctx context.Context, realm, cfgDirectory string,
) (*xconnwebrtc.WebRTCSession, error) {
	authenticator, err := clientAuthenticator(realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	webrtcSess, err := common.ConnectWebrtcSession(quicSess.Session, realm, authenticator, func() {})
	quicSess.Connection().Close()
	if err != nil {
		return nil, err
	}

	return webrtcSess, nil
}

func ConnectWebrtc(session *xconn.Session, realm, authid, privateKey string,
	onDisconnect func()) (*xconn.Session, error) {
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privateKey, map[string]any{})
	if err != nil {
		return nil, err
	}
	webrtcSess, err := common.ConnectWebrtcSession(session, realm, authenticator, onDisconnect)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

func FetchMachinesFromCloud(cfgDirectory string) ([]common.Machine, error) {
	cloudSession, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), ErrAuthenticationFailed) {
			_ = RemoveCredentialsFiles(cfgDirectory)
			return []common.Machine{}, fmt.Errorf("invalid credentials, please login again")
		}
		return []common.Machine{}, err
	}

	return fetchMachines(cloudSession)
}

// fetchMachines lists the machines on the account authenticated on session.
func fetchMachines(session *xconn.Session) ([]common.Machine, error) {
	callResp := session.Call(ProcedureListDesktop).Do()
	if callResp.Err != nil {
		return []common.Machine{}, callResp.Err
	}

	var machines []common.Machine
	jsonData, err := json.Marshal(callResp.Args())
	if err != nil {
		return []common.Machine{}, err
	}
	if err := json.Unmarshal(jsonData, &machines); err != nil {
		return []common.Machine{}, err
	}

	return machines, nil
}

// CacheMachines replaces the machines in config.yml, keeping its other sections.
func CacheMachines(cfgDirectory string, machines []common.Machine) error {
	config, err := readConfig(cfgDirectory)
	if err != nil {
		return err
	}
	config.Machines = machines
	return writeConfig(cfgDirectory, config)
}

// CallFileOp performs one key-exchange-then-encrypted-call round trip
// against machineSession -- used both by direct CLI calls and, with its own
// key cache, userd's ProxyFileOpHandler.
func CallFileOp(machineSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, err := common.ClientKeyExchange(machineSession)
	if err != nil {
		return nil, err
	}

	return common.EncryptedCall(machineSession, procedure, payload, enc)
}

// clientAuthenticator returns the authenticator this machine uses on realm's machine: the
// one for the standalone target (desk --profile), otherwise the cloud login's.
func clientAuthenticator(realm, cfgDirectory string) (auth.ClientAuthenticator, error) {
	if target, ok := common.StandaloneTargetFor(realm); ok {
		return target.Authenticator()
	}
	authid, privKey, err := common.ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}
	return xconnauth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
}
