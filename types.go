package deskconn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
	"github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

type ProxyCall struct {
	ProgressChan chan *xconn.Progress
}

type ProxyCalls struct {
	calls map[uint64]*ProxyCall
	sync.Mutex
}

func NewProxyCalls() *ProxyCalls {
	return &ProxyCalls{
		calls: make(map[uint64]*ProxyCall),
	}
}

func (p *ProxyCalls) Get(id uint64) (*ProxyCall, bool) {
	p.Lock()
	defer p.Unlock()
	c, ok := p.calls[id]
	return c, ok
}

func (p *ProxyCalls) Store(id uint64, c *ProxyCall) {
	p.Lock()
	defer p.Unlock()
	p.calls[id] = c
}

func (p *ProxyCalls) Delete(id uint64) {
	p.Lock()
	defer p.Unlock()
	delete(p.calls, id)
}

type ClientSessions struct {
	sessionByRealm map[string]*xconn.Session
	sync.Mutex
}

func NewClientSessions() *ClientSessions {
	return &ClientSessions{
		sessionByRealm: make(map[string]*xconn.Session),
	}
}

func (c *ClientSessions) SessionByRealm(authid string) (*xconn.Session, bool) {
	c.Lock()
	defer c.Unlock()
	session, ok := c.sessionByRealm[authid]
	return session, ok
}

func (c *ClientSessions) StoreSession(realm string, session *xconn.Session) {
	c.Lock()
	defer c.Unlock()
	c.sessionByRealm[realm] = session
}

func (c *ClientSessions) EnsureDeviceSession(ctx context.Context, realm, cfgDirectory string) (*xconn.Session, error) {
	if session, ok := c.SessionByRealm(realm); ok {
		return session, nil
	}

	credentialsStr, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519"))
	if err != nil {
		return nil, err
	}

	credentials := strings.Split(string(credentialsStr), " ")
	authid := strings.TrimSpace(credentials[1])
	privKey := strings.TrimSpace(credentials[0])

	session, err := xconn.ConnectCryptosign(ctx, CloudURI(), realm, authid, privKey)
	if err != nil {
		return nil, err
	}

	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
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
	}

	finalSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		log.Printf("failed to connect using webrtc: %v", err)
		finalSession = session
	}

	c.StoreSession(realm, finalSession)

	return finalSession, nil
}
