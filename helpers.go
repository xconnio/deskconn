package deskconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
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

	ProcedurePrincipalCreate = "io.xconn.deskconn.account.principal.create"
	ProcedurePrincipalDelete = "io.xconn.deskconn.account.principal.delete"
	ProcedureAccountGet      = "io.xconn.deskconn.account.get"

	ProcedureProxyShell        = "io.xconn.deskconn.deskconnd.proxy.shell"
	ProcedureProxyExec         = "io.xconn.deskconn.deskconnd.proxy.exec"
	ProcedureProxyFileOp       = "io.xconn.deskconn.deskconnd.proxy.file.op"
	ProcedureProxyDeviceInfo   = "io.xconn.deskconn.deskconnd.proxy.device.info"
	ProcedureProxyLogs         = "io.xconn.deskconn.deskconnd.proxy.logs"
	ProcedureProxyPing         = "io.xconn.deskconn.deskconnd.proxy.ping"
	ProcedureProxyCat          = "io.xconn.deskconn.deskconnd.proxy.file.cat"
	ProcedureProxyFilePush     = "io.xconn.deskconn.deskconnd.proxy.file.push"
	ProcedureProxyFilePull     = "io.xconn.deskconn.deskconnd.proxy.file.pull"
	ProcedureProxyPortForward  = "io.xconn.deskconn.deskconnd.proxy.port.forward"
	ProcedureProxyPortReverse  = "io.xconn.deskconn.deskconnd.proxy.port.reverse"
	ProcedureProxyPrinterList  = "io.xconn.deskconn.deskconnd.proxy.printer.list"
	ProcedureProxyPrinterPrint = "io.xconn.deskconn.deskconnd.proxy.printer.print"
	ProcedureLogin             = "io.xconn.deskconn.login"
	ProcedureLogout            = "io.xconn.deskconn.logout"
	ProcedureConnect           = "io.xconn.deskconn.connect"
	ProcedureDisconnect        = "io.xconn.deskconn.disconnect"
	ProcedureDisconnectAll     = "io.xconn.deskconn.disconnect_all"
	ProcedureConnectedDevices  = "io.xconn.deskconn.connected_devices"

	ProcedureListDesktop = "io.xconn.deskconn.desktop.list"

	ProcedureAppUpdateCheck = "io.xconn.deskconn.app.update.check"

	ProcedureCoturnCreate = "io.xconn.deskconn.coturn.credentials.create"

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

func Login(session *xconn.Session, username string) error {
	cfgDirectory, err := CfgDirectory()
	if err != nil {
		return err
	}

	privPath := filepath.Join(cfgDirectory, "id_ed25519")
	pubPath := filepath.Join(cfgDirectory, "id_ed25519.pub")

	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	callResp := session.Call(ProcedurePrincipalCreate).Arg(pub).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to create principal: %w", callResp.Err)
	}

	principal, err := callResp.ArgDict(0)
	if err != nil {
		return fmt.Errorf("unexpected response from principal create: %w", err)
	}
	expiresAtStr, err := principal.String("expires_at")
	if err != nil {
		return fmt.Errorf("missing expires_at in principal response from create principal: %w", err)
	}

	accountGetResp := session.Call(ProcedureAccountGet).Do()
	if accountGetResp.Err != nil {
		return fmt.Errorf("failed to create principal: %w", accountGetResp.Err)
	}
	name := accountGetResp.Args()[0].(map[string]any)["name"].(string)
	if err = os.WriteFile(privPath, []byte(priv+" "+username+" "+expiresAtStr+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	if err = os.WriteFile(pubPath, []byte(pub+" "+username+" "+name+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
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

	return xconn.ConnectCryptosign(context.Background(), CloudURI(), Realm, authid, privKey)
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

func ConnectDeviceRealm(ctx context.Context, realm, cfgDirectory string, useP2P bool) (*xconn.Session, error) {
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	session, err := xconn.ConnectCryptosign(ctx, CloudURI(), realm, authid, privKey)
	if err != nil {
		return nil, err
	}

	if !useP2P {
		return session, nil
	}

	defer func() { _ = session.Leave() }()

	return ConnectWebrtc(ctx, session, realm, authid, privKey, cfgDirectory, nil)
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
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	webrtcSess, err := ConnectWebrtc(ctx, quicSess.Session, realm, authid, privKey, cfgDirectory, func() {})
	quicSess.Connection().Close()
	if err != nil {
		return nil, err
	}

	return webrtcSess, nil
}

func ConnectWebrtc(ctx context.Context, session *xconn.Session, realm, authid, privateKey,
	cfgDirectory string, onDisconnect func()) (*xconn.Session, error) {
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privateKey, map[string]any{})
	if err != nil {
		return nil, err
	}

	iceServers, err := GetOrRefreshTURNServers(ctx, authid, privateKey, cfgDirectory)
	if err != nil {
		iceServers = []xconnwebrtc.ICEServer{
			{URLs: []string{StunServerURL}},
		}
	}

	config := &xconnwebrtc.ClientConfig{
		Realm:                    realm,
		ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  session,
		ICEServers:               iceServers,
		OnDisconnect:             onDisconnect,
	}

	finalSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		return nil, err
	}

	return finalSession, nil
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

	callResp := cloudSession.Call(ProcedureListDesktop).Do()
	if callResp.Err != nil {
		return []Device{}, err
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

func ProxyProgressiveInvocationHandler(proxyCalls *ProxyCalls, clientSessions *ClientSessions,
	cfgDirectory, procedure string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult { //nolint:contextcheck
		caller := inv.Caller()
		if !inv.Progress() {
			if proxyCall, exists := proxyCalls.Fetch(caller); exists {
				if len(inv.Args()) > 0 {
					proxyCall.send(xconn.NewFinalProgress(inv.Args()...))
				}
				proxyCall.closeChannel()
				proxyCalls.Delete(caller)
			}

			return xconn.NewInvocationResult()
		}

		proxyCall, exists := proxyCalls.Fetch(caller)
		if !exists {
			proxyCall = newProxyCall()
			proxyCalls.Store(caller, proxyCall)

			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}

			deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}

			sessionCtx, ok := clientSessions.SessionContext(realm)
			if !ok {
				sessionCtx = context.Background()
			}

			ch := proxyCall.progressChan
			go func() {
				callResp := deviceSess.Call(procedure).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-ch
						if !ok {
							return xconn.NewFinalProgress()
						}
						return p
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						_ = inv.SendProgress(pr.Args(), nil)
					}).DoContext(sessionCtx)
				if callResp.Err != nil {
					_ = inv.SendProgress([]any{[]byte(callResp.Err.Error())}, nil)
					_ = deviceSess.Leave()
					clientSessions.DeleteDeviceSession(realm)
				}
				_ = inv.SendProgress(nil, nil)
			}()
		}

		if len(inv.Args()) > 2 {
			proxyCall.send(xconn.NewProgress(inv.Args()[1:]...))
		} else {
			payload, err := inv.ArgBytes(1)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}
			proxyCall.send(xconn.NewProgress(payload))
		}
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}
}

// ProxyShellHandler proxies ProcedureShell with transparent PTY migration.
// On first connection it uses a QUIC session for fast start, then upgrades to WebRTC in
// the background. When WebRTC is ready the daemon migrates the live PTY and stores the
// WebRTC session for future reuse. If a P2P session is already cached it is used directly.
func ProxyShellHandler(proxyCalls *ProxyCalls, clientSessions *ClientSessions,
	cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()

		if !inv.Progress() {
			if proxyCall, exists := proxyCalls.Fetch(caller); exists {
				if len(inv.Args()) > 0 {
					proxyCall.send(xconn.NewFinalProgress(inv.Args()...))
				}
				proxyCall.closeChannel()
				proxyCalls.Delete(caller)
			}
			return xconn.NewInvocationResult()
		}

		var resultCh chan *xconn.InvocationResult
		migrationCtx, cancelMigration := context.WithCancel(ctx)
		defer cancelMigration()

		proxyCall, exists := proxyCalls.Fetch(caller)
		if !exists {
			proxyCall = newProxyCall()
			proxyCalls.Store(caller, proxyCall)

			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}
			initialArgs := append([]any(nil), inv.Args()[1:]...)

			tokenBytes := make([]byte, 32)
			if _, err := rand.Read(tokenBytes); err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, "failed to generate migration token")
			}
			migrationToken := hex.EncodeToString(tokenBytes)

			deviceSession, upgradeCh, err := clientSessions.EnsureDeviceSessionWithUpgrade(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}

			var migratedMu sync.Mutex
			migrated := false
			migrationStarted := false

			resultCh = make(chan *xconn.InvocationResult, 1)
			cloudCh := proxyCall.progressChan

			var closeCloudOnce sync.Once
			closeCloud := func() {
				closeCloudOnce.Do(func() {
					close(cloudCh)
				})
			}
			proxyCall.setCloseFunc(closeCloud)

			firstCloudMsg := true
			go func() {
				callResp := deviceSession.Call(ProcedureShell).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-cloudCh
						if !ok {
							return xconn.NewFinalProgress()
						}
						if firstCloudMsg {
							firstCloudMsg = false
							if p.Kwargs == nil {
								p.Kwargs = make(map[string]any)
							}
							p.Kwargs["migrate-token"] = migrationToken
						}
						return p
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						migratedMu.Lock()
						m := migrated
						started := migrationStarted
						migratedMu.Unlock()
						if !m && !started {
							_ = inv.SendProgress(pr.Args(), nil)
						}
					}).Do()

				migratedMu.Lock()
				m := migrated
				migratedMu.Unlock()
				if !m {
					if callResp.Err != nil {
						select {
						case resultCh <- xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error()):
						default:
						}
						return
					}
					select {
					case resultCh <- xconn.NewInvocationResult():
					default:
					}
				}
			}()

			// Migration goroutine: fires when QUIC upgrades to WebRTC in the background.
			go func() { //nolint:gosec
				var webrtcSession *xconn.Session
				select {
				case ws, ok := <-upgradeCh:
					if !ok || ws == nil {
						return // upgrade failed or already P2P; QUIC goroutine owns the result
					}
					webrtcSession = ws
				case <-migrationCtx.Done():
					return
				}

				newCh := make(chan *xconn.Progress, 32)
				switched := false

				migratedMu.Lock()
				migrationStarted = true
				migratedMu.Unlock()

				webrtcCallCtx, ok := clientSessions.SessionContext(realm)
				if !ok {
					webrtcCallCtx = context.Background()
				}

				// WebRTC shell call with session-id so device migrates the live PTY.
				// session-id must be in the first progress kwargs
				sessionID := deviceSession.ID()
				callResp := webrtcSession.Call(ProcedureShell).
					ProgressSender(func(_ context.Context) *xconn.Progress {
						if !switched {
							switched = true
							p := xconn.NewProgress(initialArgs...)
							p.Kwargs = map[string]any{
								"session-id":    sessionID,
								"migrate-token": migrationToken,
							}
							return p
						}
						select {
						case p, ok := <-newCh:
							if !ok {
								return xconn.NewFinalProgress()
							}
							return p
						case <-migrationCtx.Done():
							return xconn.NewFinalProgress()
						}
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						migratedMu.Lock()
						if !migrated {
							migrated = true
							proxyCall.switchChannel(newCh)
							proxyCall.setCloseFunc(nil)
							closeCloud()
						}
						migratedMu.Unlock()
						_ = inv.SendProgress(pr.Args(), nil)
					}).DoContext(webrtcCallCtx) //nolint:contextcheck

				closeCloud()
				if callResp.Err != nil {
					select {
					case resultCh <- xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error()):
					default:
					}
					clientSessions.DeleteDeviceSession(realm)
					return
				}
				select {
				case resultCh <- xconn.NewInvocationResult():
				default:
				}
			}()
		}

		if len(inv.Args()) > 2 {
			proxyCall.send(xconn.NewProgress(inv.Args()[1:]...))
		} else {
			payload, err := inv.ArgBytes(1)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}
			proxyCall.send(xconn.NewProgress(payload))
		}
		if resultCh != nil {
			return <-resultCh
		}
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}
}

type TURNCredentials struct {
	ExpiresAt  int64    `json:"expires_at"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	URLs       []string `json:"urls"`
}

func turnCredentialsFilePath(cfgDirectory string) string {
	return filepath.Join(cfgDirectory, "turn_credentials.json")
}

func loadCachedTURNCredentials(cfgDirectory string) (*TURNCredentials, error) {
	data, err := os.ReadFile(turnCredentialsFilePath(cfgDirectory))
	if err != nil {
		return nil, err
	}

	var creds TURNCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse cached TURN credentials: %w", err)
	}

	return &creds, nil
}

func saveTURNCredentials(cfgDirectory string, creds *TURNCredentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal TURN credentials: %w", err)
	}

	data = append(data, '\n')
	return os.WriteFile(turnCredentialsFilePath(cfgDirectory), data, 0600)
}

func turnCredentialsToICEServers(creds *TURNCredentials) []xconnwebrtc.ICEServer {
	return []xconnwebrtc.ICEServer{
		{URLs: []string{StunServerURL}},
		{
			URLs:           creds.URLs,
			Username:       creds.Username,
			Credential:     creds.Credential,
			CredentialType: xconnwebrtc.ICECredentialTypePassword,
		},
	}
}

func fetchTURNCredentials(session *xconn.Session) (*TURNCredentials, error) {
	resp := session.Call(ProcedureCoturnCreate).Do()
	if resp.Err != nil {
		return nil, fmt.Errorf("failed to call TURN credentials API: %w", resp.Err)
	}

	cred, err := resp.ArgDict(0)
	if err != nil {
		return nil, fmt.Errorf("invalid TURN credentials response: %w", err)
	}

	expiresAt, err := cred.Int64("expires_at")
	if err != nil {
		return nil, fmt.Errorf("missing expires_at in TURN credentials: %w", err)
	}

	username, err := cred.String("username")
	if err != nil {
		return nil, fmt.Errorf("missing username in TURN credentials: %w", err)
	}

	credential, err := cred.String("credential")
	if err != nil {
		return nil, fmt.Errorf("missing credential in TURN credentials: %w", err)
	}

	urlList, err := cred.List("urls")
	if err != nil {
		return nil, fmt.Errorf("missing urls in TURN credentials: %w", err)
	}

	turnURLs := make([]string, urlList.Len())
	for i := range urlList.Len() {
		u, err := urlList.String(i)
		if err != nil {
			return nil, fmt.Errorf("invalid url at index %d: %w", i, err)
		}
		turnURLs[i] = u
	}

	return &TURNCredentials{
		ExpiresAt:  expiresAt,
		Username:   username,
		Credential: credential,
		URLs:       turnURLs,
	}, nil
}

func FetchTURNServers(session *xconn.Session) ([]xconnwebrtc.ICEServer, int64, error) {
	creds, err := fetchTURNCredentials(session)
	if err != nil {
		return nil, 0, err
	}

	return turnCredentialsToICEServers(creds), creds.ExpiresAt, nil
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

func ProxyFilePullHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		remotePath, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		recursive, _ := inv.ArgBool(2)
		publicKey, err := inv.ArgBytes(3)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(ProcedureFileDownload).
			ProgressReceiver(func(pr *xconn.ProgressResult) {
				_ = inv.SendProgress(pr.Args(), nil)
			}).
			Args(remotePath, recursive, publicKey).DoContext(ctx)

		if callResp.Err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error())
		}
		return xconn.NewInvocationResult()
	}
}

func ProxyPortForwardHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		remotePort, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		localPort, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		if err := ForwardLocalPort(ctx, deviceSess, remotePort, localPort); err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult()
	}
}

func ProxyPortReverseHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		remotePort, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		localPort, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}

		if err := ReverseLocalPort(ctx, deviceSess, remotePort, localPort); err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
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
			go func() {
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
			}()
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

func GetOrRefreshTURNServers(ctx context.Context, authid, privKey, cfgDirectory string) ([]xconnwebrtc.ICEServer,
	error) {
	cached, err := loadCachedTURNCredentials(cfgDirectory)
	if err == nil && time.Now().Unix() < cached.ExpiresAt {
		return turnCredentialsToICEServers(cached), nil
	}

	session, err := xconn.ConnectCryptosign(ctx, CloudURI(), CloudRealm, authid, privKey)
	if err != nil {
		return nil, err
	}

	fresh, err := fetchTURNCredentials(session)
	if err != nil {
		if cached != nil {
			return turnCredentialsToICEServers(cached), nil
		}
		return nil, fmt.Errorf("failed to fetch TURN credentials: %w", err)
	}

	if saveErr := saveTURNCredentials(cfgDirectory, fresh); saveErr != nil {
		log.Printf("failed to cache TURN credentials: %v", saveErr)
	}

	return turnCredentialsToICEServers(fresh), nil
}
