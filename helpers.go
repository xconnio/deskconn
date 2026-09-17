package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/xconnio/deskconn/iptun"
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

	ProcedureProxyFileOp       = "io.xconn.deskconn.deskconnd.proxy.file.op"
	ProcedureProxyDeviceInfo   = "io.xconn.deskconn.deskconnd.proxy.device.info"
	ProcedureProxyLogs         = "io.xconn.deskconn.deskconnd.proxy.logs"
	ProcedureProxyPing         = "io.xconn.deskconn.deskconnd.proxy.ping"
	ProcedureProxyCat          = "io.xconn.deskconn.deskconnd.proxy.file.cat"
	ProcedureProxyPrinterList  = "io.xconn.deskconn.deskconnd.proxy.printer.list"
	ProcedureProxyPrinterPrint = "io.xconn.deskconn.deskconnd.proxy.printer.print"
	ProcedureProxyVPNStart     = "io.xconn.deskconn.deskconnd.proxy.vpn.start"
	ProcedureProxyVPNStop      = "io.xconn.deskconn.deskconnd.proxy.vpn.stop"
	ProcedureLogin             = "io.xconn.deskconn.login"
	ProcedureLogout            = "io.xconn.deskconn.logout"
	ProcedureConnect           = "io.xconn.deskconn.connect"
	ProcedureDisconnect        = "io.xconn.deskconn.disconnect"
	ProcedureDisconnectAll     = "io.xconn.deskconn.disconnect_all"
	ProcedureConnectedDevices  = "io.xconn.deskconn.connected_devices"

	ProcedureListDesktop = "io.xconn.deskconn.desktop.list"

	LocalRealm = "io.xconn.deskconn.local"
	CloudRealm = "io.xconn.deskconn"

	ErrAuthenticationFailed = "wamp.error.authentication_failed"

	StunServerURL = "stun:stun.l.google.com:19302"
)

var ErrKeyExpired = errors.New("authentication key expired, please login again")

func EnsureCredentials() (*Credentials, error) {
	credFilePath, err := CredentialsFilePath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(credFilePath); err != nil {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, fmt.Errorf("failed to create watcher: %w", err)
		}
		defer watcher.Close()

		if err := watcher.Add(filepath.Dir(credFilePath)); err != nil {
			return nil, fmt.Errorf("failed to add watcher: %w", err)
		}

		log.Println("Waiting for credentials file...")

		for event := range watcher.Events {
			if event.Name != credFilePath {
				continue
			}

			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				log.Println("Desktop successfully attached to cloud")
				break
			}
		}
	}

	data, err := os.ReadFile(credFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal credentials: %w", err)
	}

	return &creds, nil
}

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

func ReadPrincipalsFromFile() ([]*CryptosignPrincipal, error) {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	principalsFile := filepath.Join(cfgDirectory, "principals.json")

	data, err := os.ReadFile(principalsFile)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(string(data)) == "" {
		return []*CryptosignPrincipal{}, nil
	}

	var principals []*CryptosignPrincipal
	if err := json.Unmarshal(data, &principals); err != nil {
		return nil, err
	}

	return principals, nil
}

func WritePrincipalsToFile(principals []*CryptosignPrincipal) error {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}

	jsonData, err := json.MarshalIndent(principals, "", "  ")
	if err != nil {
		return err
	}

	jsonData = append(jsonData, '\n')

	return os.WriteFile(filepath.Join(cfgDirectory, "principals.json"), jsonData, 0600)
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

func clientKeyExchange(session *xconn.Session) (*encryptionKeys, error) {
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

	return &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey}, nil
}

func encryptedCall(session *xconn.Session, procedure string, payload []byte, enc *encryptionKeys) ([]byte, error) {
	encrypted, err := EncryptPayload(payload, enc.sendKey)
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

	return DecryptPayload(encResult, enc.receiveKey)
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

func CallFileOp(deviceSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, err := clientKeyExchange(deviceSession)
	if err != nil {
		return nil, err
	}

	return encryptedCall(deviceSession, procedure, payload, enc)
}

func ProxyFileOpHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newKeyManager()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		procedure, payload, deviceSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}

		enc, ok := km.fetch(deviceSession.ID())
		if !ok {
			var err error
			enc, err = clientKeyExchange(deviceSession)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}
			km.store(deviceSession.ID(), enc)
		}

		result, err := encryptedCall(deviceSession, procedure, payload, enc)
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

// ProxyVPNStartHandler proxies "deskconn vpn start": it arms the current Deskconn (fetched via
// getDeskconn, since deskconnd rebuilds it on every reconnect/detach cycle) to accept inbound
// VPN tunnel requests using helperSocket, a deskconn-vpnd socket the caller already started.
// deskconnd has no capability or terminal of its own for the privileged setup work.
//
// Returns as soon as arming succeeds, without blocking, so the CLI can hand off immediately.
// No tunnel can start at all until some caller has armed serving this way -- this feature's
// only gate today, in place of a real consent prompt.
func ProxyVPNStartHandler(getDeskconn func() *Deskconn) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		helperSocket, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		d := getDeskconn()
		if d == nil {
			return xconn.NewInvocationError(ErrOperationFailed, "not currently connected")
		}

		helper, err := iptun.DialClient(helperSocket)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		if err := d.ArmVPNServing(helper); err != nil {
			_ = helper.Close()
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult()
	}
}

// ProxyVPNStopHandler proxies "deskconn vpn stop": disarms serving, tearing down any active
// tunnel and closing the helper connection so deskconn-vpnd unwinds and exits. Meant to run as
// an independent command from "deskconn vpn start", not necessarily the same terminal.
func ProxyVPNStopHandler(getDeskconn func() *Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		d := getDeskconn()
		if d == nil || !d.DisarmVPNServing() {
			return xconn.NewInvocationError(ErrOperationFailed, "not currently serving")
		}
		return xconn.NewInvocationResult()
	}
}

// ProxyLogsHandler proxies ProcedureLogs using a cloud-first session with async WebRTC upgrade.
func ProxyLogsHandler(proxyCalls *ProxyCalls, clientSessions *ClientSessions,
	cfgDirectory string) xconn.InvocationHandler {
	var logStreamCounter atomic.Uint64
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()

		if !inv.Progress() {
			if proxyCall, exists := proxyCalls.Fetch(caller); exists {
				proxyCall.send(xconn.NewFinalProgress(proxyCall.streamID))
				proxyCall.closeChannel()
				proxyCalls.Delete(caller)
			}
			return xconn.NewInvocationResult()
		}

		proxyCall, exists := proxyCalls.Fetch(caller)
		if !exists {
			proxyCall = newProxyCall()
			proxyCall.streamID = logStreamCounter.Add(1)
			proxyCalls.Store(caller, proxyCall)

			realm, err := inv.ArgString(0)
			if err != nil {
				proxyCalls.Delete(caller)
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}

			deviceSession, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
			if err != nil {
				proxyCalls.Delete(caller)
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}

			ch := proxyCall.progressChan
			SafeGo(func() {
				callResp := deviceSession.Call(ProcedureLogs).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-ch
						if !ok {
							return xconn.NewFinalProgress()
						}
						return p
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						_ = inv.SendProgress(pr.Args(), nil)
					}).Do()
				if callResp.Err != nil {
					_ = inv.SendProgress([]any{[]byte(callResp.Err.Error() + "\n")}, nil)
				}
				_ = inv.SendProgress(nil, nil)
			})
		}

		args := append([]any(nil), inv.Args()[1:]...)
		args = append(args, proxyCall.streamID)
		proxyCall.send(xconn.NewProgress(args...))
		return xconn.NewInvocationError(xconn.ErrNoResult)
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
