package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/common"
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

func Login(session *xconn.Session, username, otp string) ([]common.Device, error) {
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

	devices, err := fetchDevices(cloudSession)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch devices: %w", err)
	}

	if err := CacheDevices(cfgDirectory, devices); err != nil {
		return nil, err
	}

	return devices, nil
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

	// Drop the cloud devices but keep direct devices and other settings.
	cfgPath := filepath.Join(cfgDirectory, "config.yml")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := CacheDevices(cfgDirectory, nil); err != nil {
		return os.Remove(cfgPath) // unreadable config: drop it, as before
	}
	return nil
}

// ConnectDeviceRealmP2P connects directly to a device via WebRTC P2P, using QUIC for signaling.
func ConnectDeviceRealmP2P(ctx context.Context, realm, cfgDirectory string) (*xconn.Session, error) {
	webrtcSess, err := ConnectDeviceRealmP2PSession(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

// ConnectDeviceRealmP2PSession is like ConnectDeviceRealmP2P but returns the
// full WebRTC session instead of just the WAMP session on top of it, giving
// access to the shared PeerConnection: OpenChannel (for raw, non-WAMP data
// channels such as the IP tunnel) and Connection (e.g. to inspect the
// selected ICE candidate pair).
func ConnectDeviceRealmP2PSession(ctx context.Context, realm, cfgDirectory string) (*xconnwebrtc.WebRTCSession, error) {
	authid, privKey, err := clientCredentials(realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	quicSess, err := common.ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	// The WAMP session over WebRTC joins the realm the device actually serves.
	sessionRealm := realm
	if common.IsDirectRealm(realm) {
		sessionRealm = common.StandaloneRealm
	}
	webrtcSess, err := common.ConnectWebrtcSession(quicSess.Session, sessionRealm, authid, privKey, func() {})
	quicSess.Connection().Close()
	if err != nil {
		return nil, err
	}

	return webrtcSess, nil
}

func ConnectWebrtc(session *xconn.Session, realm, authid, privateKey string,
	onDisconnect func()) (*xconn.Session, error) {
	webrtcSess, err := common.ConnectWebrtcSession(session, realm, authid, privateKey, onDisconnect)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

func FetchDevicesFromCloud(cfgDirectory string) ([]common.Device, error) {
	cloudSession, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), ErrAuthenticationFailed) {
			_ = RemoveCredentialsFiles(cfgDirectory)
			return []common.Device{}, fmt.Errorf("invalid credentials, please login again")
		}
		return []common.Device{}, err
	}

	return fetchDevices(cloudSession)
}

// fetchDevices lists the devices on the account authenticated on session.
func fetchDevices(session *xconn.Session) ([]common.Device, error) {
	callResp := session.Call(ProcedureListDesktop).Do()
	if callResp.Err != nil {
		return []common.Device{}, callResp.Err
	}

	var devices []common.Device
	jsonData, err := json.Marshal(callResp.Args())
	if err != nil {
		return []common.Device{}, err
	}
	if err := json.Unmarshal(jsonData, &devices); err != nil {
		return []common.Device{}, err
	}

	return devices, nil
}

// CacheDevices replaces the cloud devices in config.yml, keeping direct devices.
func CacheDevices(cfgDirectory string, devices []common.Device) error {
	return updateDevices(cfgDirectory, func(existing []common.Device) ([]common.Device, error) {
		for _, d := range existing {
			if d.Address != "" {
				devices = append(devices, d)
			}
		}
		return devices, nil
	})
}

// updateDevices rewrites the devices in config.yml with update, keeping its other sections.
func updateDevices(cfgDirectory string, update func([]common.Device) ([]common.Device, error)) error {
	cfgPath := filepath.Join(cfgDirectory, "config.yml")

	var config common.Config
	data, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if config.Devices, err = update(config.Devices); err != nil {
		return err
	}
	b, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return os.WriteFile(cfgPath, b, 0600)
}

// CallFileOp performs one key-exchange-then-encrypted-call round trip
// against deviceSession -- used both by direct CLI calls and, with its own
// key cache, deskconnd's ProxyFileOpHandler.
func CallFileOp(deviceSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, err := common.ClientKeyExchange(deviceSession)
	if err != nil {
		return nil, err
	}

	return common.EncryptedCall(deviceSession, procedure, payload, enc)
}
