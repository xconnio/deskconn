package deskconn

import (
	"context"
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

type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Device struct {
	Authid       string       `json:"authid"`
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Organization Organization `json:"organization"`
	Realm        string       `json:"realm"`
}

type ClientSessions struct {
	deviceSessionByRealm map[string]*xconn.Session
	sync.Mutex
}

func NewClientSessions() *ClientSessions {
	return &ClientSessions{
		deviceSessionByRealm: make(map[string]*xconn.Session),
	}
}

func (c *ClientSessions) SessionByRealm(authid string) (*xconn.Session, bool) {
	c.Lock()
	defer c.Unlock()
	session, ok := c.deviceSessionByRealm[authid]
	return session, ok
}

func (c *ClientSessions) StoreDeviceSession(realm string, session *xconn.Session) {
	c.Lock()
	defer c.Unlock()
	c.deviceSessionByRealm[realm] = session
}

func (c *ClientSessions) EnsureDeviceSession(ctx context.Context, realm, cfgDirectory string) (*xconn.Session, error) {
	if session, ok := c.SessionByRealm(realm); ok {
		return session, nil
	}

	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

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
		return session, nil
	}

	c.StoreDeviceSession(realm, finalSession)

	return finalSession, nil
}
