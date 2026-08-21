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

	ProcedurePrincipalDelete    = "io.xconn.deskconn.account.principal.delete"
	ProcedureAccountGet         = "io.xconn.deskconn.account.get"
	ProcedureAccountLogin       = "io.xconn.deskconn.account.login"
	ProcedureAccountLoginVerify = "io.xconn.deskconn.account.login.verify"

	ProcedureProxyShell        = "io.xconn.deskconn.deskconnd.proxy.shell"
	ProcedureProxyShellMigrate = "io.xconn.deskconn.deskconnd.proxy.shell.migrate"
	ProcedureProxyAgentForward = "io.xconn.deskconn.deskconnd.proxy.agent.forward"
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

func Login(session *xconn.Session, username, otp string) error {
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

	callResp := session.Call(ProcedureAccountLoginVerify).Args(username, otp, pub).Do()
	if callResp.Err != nil {
		return fmt.Errorf("failed to verify otp: %w", callResp.Err)
	}

	principal, err := callResp.ArgDict(0)
	if err != nil {
		return fmt.Errorf("unexpected response from login verify: %w", err)
	}
	expiresAtStr, err := principal.String("expires_at")
	if err != nil {
		return fmt.Errorf("missing expires_at in login verify response: %w", err)
	}

	accountGetResp := session.Call(ProcedureAccountGet).Do()
	if accountGetResp.Err != nil {
		return fmt.Errorf("failed to get account: %w", accountGetResp.Err)
	}
	account, err := accountGetResp.ArgDict(0)
	if err != nil {
		return fmt.Errorf("unexpected response from account get: %w", err)
	}
	name, err := account.String("name")
	if err != nil {
		return fmt.Errorf("missing name in account get response: %w", err)
	}
	if err = os.WriteFile(privPath, []byte(priv+" "+username+" "+expiresAtStr+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	if err = os.WriteFile(pubPath, []byte(pub+" "+username+" "+name+"\n"), 0600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	devices, err := FetchDevicesFromCloud(cfgDirectory)
	if err != nil {
		return fmt.Errorf("failed to fetch devices: %w", err)
	}

	devicesYAML, err := yaml.Marshal(Config{Devices: devices})
	if err != nil {
		return fmt.Errorf("failed to marshal devices: %w", err)
	}

	if err = os.WriteFile(filepath.Join(cfgDirectory, "config.yml"), devicesYAML, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
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
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	webrtcSess, err := ConnectWebrtc(quicSess.Session, realm, authid, privKey, func() {})
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

	finalSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		return nil, err
	}

	return finalSession.Session, nil
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
			SafeGo(func() {
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
			})
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

			SafeGo(func() {
				callResp := deviceSession.Call(ProcedureShell).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-cloudCh
						if !ok {
							return xconn.NewFinalProgress()
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
			})

			// Migration goroutine: fires when QUIC upgrades to WebRTC in the background.
			SafeGo(func() { //nolint:gosec
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
				// The migrate-token itself was generated device-side and relayed to us by
				// the client still encrypted (see ProxyShellMigrateHandler) — we never see
				// the plaintext, we just carry it along with our own session-id bookkeeping.
				sessionID := deviceSession.ID()
				var migrateBlob []byte
				select {
				case migrateBlob = <-proxyCall.migrateBlobCh:
				case <-migrationCtx.Done():
					return
				}
				callResp := webrtcSession.Call(ProcedureShell).
					ProgressSender(func(_ context.Context) *xconn.Progress {
						if !switched {
							switched = true
							p := xconn.NewProgress(initialArgs...)
							p.Kwargs = map[string]any{
								"session-id": sessionID,
								"migrate":    migrateBlob,
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
			})
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

// ProxyShellMigrateHandler receives the migration token the device generated and encrypted
// for the caller's own shell session.
func ProxyShellMigrateHandler(proxyCalls *ProxyCalls) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		blob, err := inv.ArgBytes(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		if proxyCall, exists := proxyCalls.Fetch(inv.Caller()); exists {
			proxyCall.setMigrateBlob(blob)
		}
		return xconn.NewInvocationResult()
	}
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

		err = ForwardLocalPort(ctx, deviceSess, remotePort, localPort)
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
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
