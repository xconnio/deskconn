package deskconn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
	xconnwebrtc "github.com/xconnio/xconn-core/bindings/go/webrtc"
)

const ErrAuthenticationFailed = "wamp.error.authentication_failed"

// Login signs username in to the cloud with a new key, keeps the key in cfgDirectory and
// caches their machines, which it returns.
func Login(ctx context.Context, username, password string) ([]common.Machine, error) {
	c, err := currentCloud()
	if err != nil {
		return nil, err
	}
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

	expiresAt, err := c.Login(ctx, username, password, pub)
	if err != nil {
		return nil, fmt.Errorf("failed to login: %w", err)
	}

	cloudSession, err := ConnectCloudCryptosign(ctx, username, priv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cloudSession.Leave() }()

	name, err := c.Name(cloudSession)
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	privLine := priv + " " + username
	if !expiresAt.IsZero() {
		privLine += " " + expiresAt.Format(time.RFC3339Nano)
	}
	if err = os.WriteFile(privPath, []byte(privLine+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	if err = os.WriteFile(pubPath, []byte(pub+" "+username+" "+name+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	machines, err := c.Machines(cloudSession)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch machines: %w", err)
	}

	if err := CacheMachines(cfgDirectory, machines); err != nil {
		return nil, err
	}

	return machines, nil
}

// Logout revokes the login's key on the cloud and removes it. With the key already
// rejected or expired, it only removes it.
func Logout(cfgDirectory string) error {
	c, err := currentCloud()
	if err != nil {
		return err
	}
	cloudSession, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), ErrAuthenticationFailed) ||
			errors.Is(err, common.ErrKeyExpired) {
			return RemoveCredentialsFiles(cfgDirectory)
		}
		return err
	}
	defer func() { _ = cloudSession.Leave() }()

	pubKey, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519.pub"))
	if err != nil {
		return err
	}
	if err := c.Logout(cloudSession, strings.Split(string(pubKey), " ")[0]); err != nil {
		return err
	}

	return RemoveCredentialsFiles(cfgDirectory)
}

func ConnectCloudRealm(cfgDirectory string) (*xconn.Session, error) {
	authid, privKey, err := common.ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	return ConnectCloudCryptosign(context.Background(), authid, privKey)
}

func ConnectCloudCryptosign(ctx context.Context, authid, privKey string) (*xconn.Session, error) {
	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticator: %w", err)
	}

	quicSess, err := xconn.ConnectQUIC(ctx, common.CloudQUICAddress(), common.Realm,
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
	authenticator := auth.NewWAMPCRAAuthenticator(username, password, nil)
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

	webrtcSess, err := common.ConnectWebrtcSession(quicSess.Session, common.ServedRealm(realm), authenticator, func() {})
	quicSess.Connection().Close()
	if err != nil {
		return nil, err
	}

	return webrtcSess, nil
}

func ConnectWebrtc(session *xconn.Session, realm, authid, privateKey string,
	onDisconnect func()) (*xconn.Session, error) {
	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privateKey, map[string]any{})
	if err != nil {
		return nil, err
	}
	webrtcSess, err := common.ConnectWebrtcSession(session, realm, authenticator, onDisconnect)
	if err != nil {
		return nil, err
	}

	return webrtcSess.Session, nil
}

// FetchMachinesFromCloud lists the machines of the signed in user.
func FetchMachinesFromCloud(cfgDirectory string) ([]common.Machine, error) {
	c, err := currentCloud()
	if err != nil {
		return []common.Machine{}, err
	}
	cloudSession, err := ConnectCloudRealm(cfgDirectory)
	if err != nil {
		if strings.Contains(err.Error(), ErrAuthenticationFailed) {
			_ = RemoveCredentialsFiles(cfgDirectory)
			return []common.Machine{}, fmt.Errorf("invalid credentials, please login again")
		}
		return []common.Machine{}, err
	}
	defer func() { _ = cloudSession.Leave() }()

	return c.Machines(cloudSession)
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

// RefreshMachines fetches the account's machines from the cloud, keeping the aliases set on
// cached (the machines in config.yml) and telling machines of the same name apart by their
// ID. Callers cache the result (CacheMachines).
func RefreshMachines(cfgDirectory string, cached []common.Machine) ([]common.Machine, error) {
	machines, err := FetchMachinesFromCloud(cfgDirectory)
	if err != nil {
		return nil, err
	}

	cfgMap := make(map[string]common.Machine, len(cached))
	for _, d := range cached {
		cfgMap[d.Authid] = d
	}

	nameCount := make(map[string]int)
	for i, d := range machines {
		count := nameCount[d.Name]
		if count > 0 {
			newName := fmt.Sprintf("%s-%s", d.Name, d.Authid[:6])
			machines[i].Name = newName
		}
		if localMachine, ok := cfgMap[d.Authid]; ok {
			machines[i].Alias = localMachine.Alias
		}
		nameCount[machines[i].Name]++
	}

	return machines, nil
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
	return auth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
}
