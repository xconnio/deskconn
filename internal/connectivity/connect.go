package connectivity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
	auth "github.com/xconnio/xconn/bindings/go/auth"
	xconnwebrtc "github.com/xconnio/xconn/bindings/go/webrtc"
)

// UnixSocketURI builds the local yamux URI on Unix systems.
// Use ConnectDaemon for a connection that also supports Windows.
func UnixSocketURI(path string) string {
	return "unix+yamux://" + path
}

// ConnectMachineRealmQUIC connects to realm's machine: directly over QUIC or WebTransport for
// a standalone machine the CLI resolved (see SetStandaloneTargetFor), otherwise through
// the cloud over QUIC. Every call stays on that connection.
func ConnectMachineRealmQUIC(ctx context.Context, realm, cfgDirectory string) (*MachineConn, error) {
	return connectMachineRealm(ctx, realm, cfgDirectory, true)
}

// ConnectMachineRealmManaged is ConnectMachineRealmQUIC with XConn choosing the path of each
// new call to a cloud machine: a direct WebRTC session once the machine has one, negotiated in
// the background, otherwise the cloud connection, without waiting for it.
func ConnectMachineRealmManaged(ctx context.Context, realm, cfgDirectory string) (*MachineConn, error) {
	return connectMachineRealm(ctx, realm, cfgDirectory, false)
}

func connectMachineRealm(ctx context.Context, realm, cfgDirectory string, routedOnly bool) (*MachineConn, error) {
	if target, ok := common.StandaloneTargetFor(realm); ok {
		authenticator, err := target.Authenticator()
		if err != nil {
			return nil, err
		}
		return ConnectStandalone(ctx, target.URL, target.CertHash, common.ServedRealm(realm), authenticator)
	}

	authid, privKey, err := common.ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticator: %w", err)
	}

	connection, err := xconn.DialQUICWithTLS(ctx, common.CloudQUICAddress(), common.CloudQUICTLSConfig())
	if err != nil {
		return nil, err
	}
	client := &xconn.Client{Authenticator: authenticator, SerializerSpec: xconn.CBORSerializerSpec, RoutedOnly: routedOnly}
	if !routedOnly {
		client.Direct = &xconn.DirectConfig{ICEServers: ICEServers()}
	}
	session, err := connection.OpenSession(ctx, realm, client)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &MachineConn{Session: session, closeConn: connection.Close, close: session.Close}, nil
}

// ICEServers returns the STUN server plus, when DESKCONN_TURN_URL is set, a TURN server
// authenticated with DESKCONN_TURN_USERNAME and DESKCONN_TURN_PASSWORD.
// DESKCONN_TURN_URL may hold several comma-separated URLs.
func ICEServers() []xconnwebrtc.ICEServer {
	servers := []xconnwebrtc.ICEServer{{URLs: []string{common.StunServerURL}}}

	var urls []string
	for _, u := range strings.Split(os.Getenv("DESKCONN_TURN_URL"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return servers
	}

	return append(servers, xconnwebrtc.ICEServer{
		URLs:       urls,
		Username:   os.Getenv("DESKCONN_TURN_USERNAME"),
		Credential: os.Getenv("DESKCONN_TURN_PASSWORD"),
	})
}

// ConnectWebrtcSession negotiates a direct WebRTC session using WAMP signaling.
// The Rust transport closes failed peer connections and their data channels.
func ConnectWebrtcSession(session *xconn.Session, realm string, authenticator auth.ClientAuthenticator,
	onDisconnect func()) (*xconnwebrtc.WebRTCSession, error) {
	config := &xconnwebrtc.ClientConfig{
		Realm:                    realm,
		ProcedureWebRTCOffer:     common.ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: common.TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  common.TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  session,
		ICEServers:               ICEServers(),
		OnDisconnect: func() {
			if onDisconnect != nil {
				onDisconnect()
			}
		},
	}

	webrtcSess, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		return nil, err
	}
	return webrtcSess, nil
}

// DaemonURI is the local Unix socket URI. Use ConnectDaemon on all platforms.
func DaemonURI(cfgDirectory string) string {
	return UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// ConnectDaemon joins the current user's local daemon on each supported platform.
func ConnectDaemon(ctx context.Context, cfgDirectory string) (*xconn.Session, error) {
	return ConnectApp(ctx, filepath.Join(cfgDirectory, "deskconn.sock"), common.LocalRealm)
}
