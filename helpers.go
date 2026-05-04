package deskconn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	ProcedureProxyShell       = "io.xconn.deskconn.deskconnd.proxy.shell"
	ProcedureProxyExec        = "io.xconn.deskconn.deskconnd.proxy.exec"
	ProcedureLogin            = "io.xconn.deskconn.login"
	ProcedureLogout           = "io.xconn.deskconn.logout"
	ProcedureConnectedDevices = "io.xconn.deskconn.connected_devices"

	ProcedureListDesktop = "io.xconn.deskconn.desktop.list"

	ProcedureAppUpdateCheck = "io.xconn.deskconn.app.update.check"

	ProcedureCoturnCreate = "io.xconn.deskconn.coturn.credentials.create"

	LocalRealm = "io.xconn.deskconn.local"
	CloudRealm = "io.xconn.deskconn"

	ErrAuthenticationFailed = "wamp.error.authentication_failed"

	StunServerURL = "stun:stun.l.google.com:19302"
)

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

	accountGetResp := session.Call(ProcedureAccountGet).Do()
	if accountGetResp.Err != nil {
		return fmt.Errorf("failed to create principal: %w", callResp.Err)
	}
	name := accountGetResp.Args()[0].(map[string]any)["name"].(string)
	if err = os.WriteFile(privPath, []byte(priv+" "+username+"\n"), 0600); err != nil {
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
	authid := strings.TrimSpace(credentials[1])
	privKey := strings.TrimSpace(credentials[0])

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

	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
	if err != nil {
		return nil, err
	}

	iceServers, err := GetOrRefreshTURNServers(ctx, authid, privKey, cfgDirectory)
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
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()
		if !inv.Progress() {
			if pc, exists := proxyCalls.Get(caller); exists {
				close(pc.ProgressChan)
				proxyCalls.Delete(caller)
			}

			return xconn.NewInvocationResult()
		}

		payload, err := inv.ArgBytes(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		pc, exists := proxyCalls.Get(caller)
		if !exists {
			progressChan := make(chan *xconn.Progress, 32)

			pc = &ProxyCall{
				ProgressChan: progressChan,
			}
			proxyCalls.Store(caller, pc)

			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}

			deviceSession, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}

			go func() {
				callResp := deviceSession.Call(procedure).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-progressChan
						if !ok {
							return xconn.NewFinalProgress()
						}
						return p
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						if len(pr.Args()) > 0 {
							_ = inv.SendProgress(pr.Args(), nil)
						} else {
							progressChan <- xconn.NewFinalProgress()
						}
					}).Do()
				if callResp.Err != nil {
					_ = inv.SendProgress([]any{[]byte(callResp.Err.Error())}, nil)
					_ = deviceSession.Leave()
					clientSessions.DeleteDeviceSession(realm)
				}
				_ = inv.SendProgress(nil, nil)
			}()
		}

		if len(inv.Args()) > 2 {
			pc.ProgressChan <- xconn.NewProgress(inv.Args()[1:]...)
		} else {
			pc.ProgressChan <- xconn.NewProgress(payload)
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
