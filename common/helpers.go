package common

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
	xconnauth "github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

var ErrKeyExpired = errors.New("authentication key expired, please login again")

func CredentialsFilePath() (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home dir: %w", err)
	}

	credFilePath := filepath.Join(homedir, ".deskconn/credentials.json")

	_ = os.MkdirAll(filepath.Dir(credFilePath), 0755)

	return credFilePath, nil
}

func CfgDirectory() (string, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home dir: %w", err)
	}

	cfgDirectory := filepath.Join(homedir, ".deskconn")

	_ = os.MkdirAll(cfgDirectory, 0755)
	return cfgDirectory, nil
}

func DevicesFromCfg(cfgDirectory string) ([]Device, error) {
	data, err := os.ReadFile(filepath.Join(cfgDirectory, "config.yml"))
	if err != nil {
		return []Device{}, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return []Device{}, err
	}

	return config.Devices, nil
}

func ReadCredentials(cfgDirectory string) (string, string, error) {
	path := filepath.Join(cfgDirectory, "id_ed25519")

	credentialsStr, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("user not logged in")
		}
		return "", "", err
	}

	credentials := strings.Split(string(credentialsStr), " ")
	if len(credentials) < 2 {
		return "", "", fmt.Errorf("malformed credentials file: %s", path)
	}
	privKey := strings.TrimSpace(credentials[0])
	authid := strings.TrimSpace(credentials[1])

	if len(credentials) >= 3 {
		expiresAtStr := strings.TrimSpace(credentials[2])
		expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtStr)
		if err == nil && time.Now().After(expiresAt) {
			return "", "", fmt.Errorf("%w", ErrKeyExpired)
		}
	}

	return authid, privKey, nil
}

// ConnectDeviceRealmQUIC connects to realm's device: over yamux for the standalone target
// the CLI was pointed at (see SetStandaloneTarget), otherwise through the cloud over QUIC.
func ConnectDeviceRealmQUIC(ctx context.Context, realm, cfgDirectory string) (*DeviceConn, error) {
	if target, ok := StandaloneTargetFor(realm); ok {
		authenticator, err := target.Authenticator()
		if err != nil {
			return nil, err
		}
		return ConnectYamux(ctx, target.URL, realm, authenticator)
	}

	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticator: %w", err)
	}

	sess, err := xconn.ConnectQUIC(ctx, CloudQUICAddress(), realm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     CloudQUICTLSConfig(),
	})
	if err != nil {
		return nil, err
	}
	return NewQUICDeviceConn(sess), nil
}

// ConnectWebrtcSession negotiates a WebRTC session to realm's device, signaling over
// session, and joins realm over it with authenticator.
func ConnectWebrtcSession(session *xconn.Session, realm string, authenticator auth.ClientAuthenticator,
	onDisconnect func()) (*xconnwebrtc.WebRTCSession, error) {
	config := &xconnwebrtc.ClientConfig{
		Realm:                    realm,
		ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  session,
		ICEServers: []xconnwebrtc.ICEServer{
			{URLs: []string{StunServerURL}},
		},
		OnDisconnect: onDisconnect,
	}

	return xconnwebrtc.ConnectWAMP(config)
}

// SessionKeys is one X25519 key exchange's derived send/receive keys, from
// the calling (client) side's perspective.
type SessionKeys struct {
	SendKey    []byte
	ReceiveKey []byte
}

// ClientKeyExchange performs a key exchange against session's
// ProcedureKeyExchange, as the calling side.
func ClientKeyExchange(session *xconn.Session) (*SessionKeys, error) {
	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return nil, err
	}

	keyResp := session.Call(ProcedureKeyExchange).Args(publicKey).Do()
	if keyResp.Err != nil {
		return nil, keyResp.Err
	}

	serverPublicKey, err := keyResp.ArgBytes(0)
	if err != nil {
		return nil, err
	}

	sendKey, receiveKey, err := ClientKeyExchangeKeys(privateKey, serverPublicKey)
	if err != nil {
		return nil, err
	}

	return &SessionKeys{SendKey: sendKey, ReceiveKey: receiveKey}, nil
}

// EncryptedCall encrypts payload with enc, calls procedure with it, and
// decrypts the single-argument encrypted result.
func EncryptedCall(session *xconn.Session, procedure string, payload []byte, enc *SessionKeys) ([]byte, error) {
	encrypted, err := EncryptPayload(payload, enc.SendKey)
	if err != nil {
		return nil, err
	}

	opResp := session.Call(procedure).Args(encrypted).Do()
	if opResp.Err != nil {
		return nil, opResp.Err
	}

	encResult, err := opResp.ArgBytes(0)
	if err != nil {
		return nil, err
	}

	return DecryptPayload(encResult, enc.ReceiveKey)
}

// SafeGo runs f in a new goroutine, recovering any panic so a bug in one
// background task (a file transfer, port-forward, shell session, etc.)
// cannot take down the whole process.
func SafeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("recovered panic in goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		f()
	}()
}
