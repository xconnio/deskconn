package common

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/internal/connectivity"
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
// the cloud over QUIC.
func ConnectMachineRealmQUIC(ctx context.Context, realm, cfgDirectory string) (*MachineConn, error) {
	if target, ok := StandaloneTargetFor(realm); ok {
		authenticator, err := target.Authenticator()
		if err != nil {
			return nil, err
		}
		return ConnectStandalone(ctx, target.URL, target.CertHash, ServedRealm(realm), authenticator)
	}

	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, nil)
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
	return NewQUICMachineConn(sess), nil
}

// ICEServers returns the STUN server plus, when DESKCONN_TURN_URL is set, a TURN server
// authenticated with DESKCONN_TURN_USERNAME and DESKCONN_TURN_PASSWORD.
// DESKCONN_TURN_URL may hold several comma-separated URLs.
func ICEServers() []xconnwebrtc.ICEServer {
	servers := []xconnwebrtc.ICEServer{{URLs: []string{StunServerURL}}}

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
		ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  TopicOffererOnCandidate,
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

// cloudQUICAddress is the default cloud router. Self-hosted builds override it with
// -ldflags "-X github.com/xconnio/deskconn/common.cloudQUICAddress=host:port".
var cloudQUICAddress = "api.deskconn.com:8081" //nolint:gochecknoglobals // set at build time via -ldflags

func CloudQUICAddress() string {
	if v, ok := os.LookupEnv("DESKCONN_CLOUD_QUIC_ADDRESS"); ok {
		return v
	}
	return cloudQUICAddress
}

func CloudQUICTLSConfig() *tls.Config {
	host, _, err := net.SplitHostPort(CloudQUICAddress())
	if err == nil {
		switch host {
		case "0.0.0.0", "127.0.0.1", "::1", "localhost":
			return &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		}
	}
	return nil
}

// DaemonURI is the local Unix socket URI. Use ConnectDaemon on all platforms.
func DaemonURI(cfgDirectory string) string {
	return UnixSocketURI(filepath.Join(cfgDirectory, "deskconn.sock"))
}

// ConnectDaemon joins the current user's local daemon on each supported platform.
func ConnectDaemon(ctx context.Context, cfgDirectory string) (*xconn.Session, error) {
	return connectivity.ConnectApp(ctx, filepath.Join(cfgDirectory, "deskconn.sock"), LocalRealm)
}
