package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
	xconnauth "github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

const (
	ProcedureWebRTCOffer     = "io.xconn.webrtc.offer"
	TopicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"
	TopicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"

	ProcedurePrincipalDelete    = "io.xconn.deskconn.account.principal.delete"
	ProcedureAccountGet         = "io.xconn.deskconn.account.get"
	ProcedureAccountLogin       = "io.xconn.deskconn.account.login"
	ProcedureAccountLoginVerify = "io.xconn.deskconn.account.login.verify"

	ProcedureListDesktop = "io.xconn.deskconn.desktop.list"

	CloudRealm = "io.xconn.deskconn"

	ErrAuthenticationFailed = "wamp.error.authentication_failed"

	StunServerURL = "stun:stun.l.google.com:19302"
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

// UnixSocketURI builds a "unix://" URI for a local unix-domain-socket path, for xconn.Connect*
// functions -- which parse the URI with net/url and dial its .Path field directly.
//
// On Windows, net/url unconditionally prepends "/" to a hierarchical URI's path, and Windows'
// AF_UNIX implementation rejects that leading slash once it's followed by a drive letter (e.g.
// "/C:/Users/..."), regardless of which slash direction the rest of the path uses. Stripping
// the drive letter and converting to forward slashes works around this: Windows resolves a
// driveless absolute path against the dialing process's current drive, which is always correct
// here since every caller builds path from CfgDirectory (the current user's own profile
// directory) - always on the same drive as whatever deskconn process is doing the dialing.
func UnixSocketURI(path string) string {
	if runtime.GOOS == "windows" {
		if len(path) >= 2 && path[1] == ':' {
			path = path[2:]
		}
		path = strings.ReplaceAll(path, `\`, "/")
	}
	return "unix://" + path
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

func Login(session *xconn.Session, username, otp string) ([]Device, error) {
	cfgDirectory, err := CfgDirectory()
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

func ConnectCloudRealm(cfgDirectory string) (*xconn.Session, error) {
	authid, privKey, err := ReadCredentials(cfgDirectory)
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

	quicSess, err := xconn.ConnectQUIC(context.Background(), CloudQUICAddress(), Realm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     CloudQUICTLSConfig(),
	})
	if err != nil {
		return nil, err
	}

	SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	return quicSess.Session, nil
}

func ConnectCloudCRA(ctx context.Context, username, password string) (*xconn.QUICSession, error) {
	authenticator := xconnauth.NewWAMPCRAAuthenticator(username, password, nil)
	return xconn.ConnectQUIC(ctx, CloudQUICAddress(), Realm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     CloudQUICTLSConfig(),
	})
}

func RemoveCredentialsFiles(cfgDirectory string) error {
	files := []string{
		filepath.Join(cfgDirectory, "id_ed25519"),
		filepath.Join(cfgDirectory, "id_ed25519.pub"),
		filepath.Join(cfgDirectory, "config.yml"),
	}

	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func ConnectDeviceRealmQUIC(ctx context.Context, realm, cfgDirectory string) (*xconn.QUICSession, error) {
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticator: %w", err)
	}

	return xconn.ConnectQUIC(ctx, CloudQUICAddress(), realm, &xconn.QUICDialerConfig{
		Authenticator: authenticator,
		TLSConfig:     CloudQUICTLSConfig(),
	})
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
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	webrtcSess, err := connectWebrtcSession(quicSess.Session, realm, authid, privKey, func() {})
	quicSess.Connection().Close()
	if err != nil {
		return nil, err
	}

	return webrtcSess, nil
}

func ConnectWebrtc(session *xconn.Session, realm, authid, privateKey string,
	onDisconnect func()) (*xconn.Session, error) {
	webrtcSess, err := connectWebrtcSession(session, realm, authid, privateKey, onDisconnect)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

func connectWebrtcSession(session *xconn.Session, realm, authid, privateKey string,
	onDisconnect func()) (*xconnwebrtc.WebRTCSession, error) {
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privateKey, map[string]any{})
	if err != nil {
		return nil, err
	}

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

func FetchDevicesFromCloud(cfgDirectory string) ([]Device, error) {
	cloudSession, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), ErrAuthenticationFailed) {
			_ = RemoveCredentialsFiles(cfgDirectory)
			return []Device{}, fmt.Errorf("invalid credentials, please login again")
		}
		return []Device{}, err
	}

	return fetchDevices(cloudSession)
}

// fetchDevices lists the devices on the account authenticated on session.
func fetchDevices(session *xconn.Session) ([]Device, error) {
	callResp := session.Call(ProcedureListDesktop).Do()
	if callResp.Err != nil {
		return []Device{}, callResp.Err
	}

	var devices []Device
	jsonData, err := json.Marshal(callResp.Args())
	if err != nil {
		return []Device{}, err
	}
	if err := json.Unmarshal(jsonData, &devices); err != nil {
		return []Device{}, err
	}

	return devices, nil
}

func CacheDevices(cfgDirectory string, devices []Device) error {
	devicesYAML, err := yaml.Marshal(Config{Devices: devices})
	if err != nil {
		return fmt.Errorf("failed to marshal devices: %w", err)
	}

	if err := os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), devicesYAML, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
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

func parseFileProxyArgs(ctx context.Context, inv *xconn.Invocation, clientSessions *ClientSessions,
	cfgDirectory string) (strArg string, bytesArg []byte, sess *xconn.Session, invErr *xconn.InvocationResult) {
	realm, err := inv.ArgString(0)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	strArg, err = inv.ArgString(1)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	bytesArg, err = inv.ArgBytes(2)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	sess, err = clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return strArg, bytesArg, sess, nil
}

// CallFileOp performs one key-exchange-then-encrypted-call round trip
// against deviceSession -- used both by direct CLI calls and, with its own
// key cache, deskconnd's ProxyFileOpHandler.
func CallFileOp(deviceSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, err := ClientKeyExchange(deviceSession)
	if err != nil {
		return nil, err
	}

	return EncryptedCall(deviceSession, procedure, payload, enc)
}

// proxyKeyCache caches the client-role SessionKeys ProxyFileOpHandler
// derives per outbound device session, so repeated proxied calls to the
// same device don't re-run the key exchange every time.
type proxyKeyCache struct {
	mu   sync.Mutex
	keys map[uint64]*SessionKeys
}

func newProxyKeyCache() *proxyKeyCache {
	return &proxyKeyCache{keys: make(map[uint64]*SessionKeys)}
}

func (c *proxyKeyCache) fetch(sessionID uint64) (*SessionKeys, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	enc, ok := c.keys[sessionID]
	return enc, ok
}

func (c *proxyKeyCache) store(sessionID uint64, enc *SessionKeys) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[sessionID] = enc
}

func ProxyFileOpHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newProxyKeyCache()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		procedure, payload, deviceSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}

		enc, ok := km.fetch(deviceSession.ID())
		if !ok {
			var err error
			enc, err = ClientKeyExchange(deviceSession)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
			km.store(deviceSession.ID(), enc)
		}

		result, err := EncryptedCall(deviceSession, procedure, payload, enc)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult(result)
	}
}

func ProxyDeviceInfoHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(ProcedureDeviceInfo).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

func ProxyPingHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		start := time.Now()
		callResp := deviceSess.Call(ProcedurePing).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(time.Since(start).Milliseconds())
	}
}

func ProxyPrinterListHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(ProcedurePrinterList).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

func ProxyPrinterPrintHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		printer, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		filename, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		data, err := inv.ArgBytes(3)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(ProcedurePrinterPrint).Args(printer, filename, data).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

func ProxyCatHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		remotePath, publicKey, deviceSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}

		callResp := deviceSession.Call(ProcedureFileCat).
			ProgressReceiver(func(pr *xconn.ProgressResult) {
				_ = inv.SendProgress(pr.Args(), nil)
			}).
			Args(remotePath, publicKey).
			DoContext(ctx)

		if callResp.Err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}
		return xconn.NewInvocationResult()
	}
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
