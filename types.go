package deskconn

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
	"github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

type ProxyCall struct {
	sync.Mutex
	progCh chan *xconn.Progress
}

func newProxyCall() *ProxyCall {
	return &ProxyCall{progCh: make(chan *xconn.Progress, 32)}
}

func (pc *ProxyCall) send(p *xconn.Progress) {
	pc.Lock()
	ch := pc.progCh
	pc.Unlock()
	ch <- p
}

func (pc *ProxyCall) switchCh(newCh chan *xconn.Progress) {
	pc.Lock()
	pc.progCh = newCh
	pc.Unlock()
}

func (pc *ProxyCall) closeCh() {
	pc.Lock()
	ch := pc.progCh
	pc.Unlock()
	close(ch)
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
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name" yaml:"name"`
}

type Device struct {
	Authid       string       `json:"authid" yaml:"authid"`
	ID           string       `json:"id" yaml:"id"`
	Name         string       `json:"name" yaml:"name"`
	Organization Organization `json:"organization" yaml:"organization"`
	Realm        string       `json:"realm" yaml:"realm"`
	Alias        string       `yaml:"alias"`
	Connected    bool         `yaml:"-" json:"-"`
}

type PrintingConfig struct {
	Mode PrintMode `yaml:"mode,omitempty"`
}

type Config struct {
	Devices  []Device       `yaml:"devices"`
	Printing PrintingConfig `yaml:"printing,omitempty"`
}

type ClientSessions struct {
	deviceSessionByRealm map[string]*xconn.Session
	loggedIn             bool
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
	c.deviceSessionByRealm[realm] = session
	c.Unlock()
}

func (c *ClientSessions) DeviceSessions() map[string]*xconn.Session {
	c.Lock()
	defer c.Unlock()
	return c.deviceSessionByRealm
}

func (c *ClientSessions) DeleteDeviceSession(realm string) {
	c.Lock()
	delete(c.deviceSessionByRealm, realm)
	c.Unlock()
}

// EnsureDeviceSessionWithUpgrade is like EnsureDeviceSession but also returns a channel
// that receives the WebRTC session when the background upgrade completes. If a connected
// session is already cached the channel is closed immediately.
func (c *ClientSessions) EnsureDeviceSessionWithUpgrade(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, <-chan *xconn.Session, error) {
	if session, ok := c.SessionByRealm(realm); ok {
		if session.Connected() {
			ch := make(chan *xconn.Session)
			close(ch)
			return session, ch, nil
		}
		c.DeleteDeviceSession(realm)
	}

	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, nil, err
	}

	cloudSession, err := xconn.ConnectCryptosign(ctx, CloudURI(), realm, authid, privKey)
	if err != nil {
		return nil, nil, err
	}

	upgradeCh := make(chan *xconn.Session, 1)
	go func() { // nolint:contextcheck
		if sess := c.upgradeToWebRTC(cloudSession, authid, privKey, realm); sess != nil {
			upgradeCh <- sess
		}
		close(upgradeCh)
	}()

	return cloudSession, upgradeCh, nil
}

// upgradeToWebRTC negotiates a WebRTC session using cloudSession for signalling. On
// success it atomically replaces the stored session, starts the reconnect loop, and
// returns the WebRTC session. Returns nil on failure.
func (c *ClientSessions) upgradeToWebRTC(cloudSession *xconn.Session, authid, privKey, realm string) *xconn.Session {
	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
	if err != nil {
		log.Printf("webrtc upgrade: failed to create authenticator: %v", err)
		return nil
	}

	config := &xconnwebrtc.ClientConfig{
		Realm:                    realm,
		ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  cloudSession,
		ICEServers:               []xconnwebrtc.ICEServer{{URLs: []string{StunServerURL}}},
	}

	webrtcSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		log.Printf("webrtc upgrade: failed to connect: %v", err)
		return nil
	}

	c.StoreDeviceSession(realm, webrtcSession)

	log.Printf("webrtc upgrade: session for %s upgraded to WebRTC", realm)
	go c.reconnectLoop(webrtcSession, authid, privKey, realm) //nolint
	return webrtcSession
}

// EnsureP2PDeviceSession returns the cached session if one exists. Otherwise it
// establishes a WebRTC connection synchronously — never falling back to cloud.
func (c *ClientSessions) EnsureP2PDeviceSession(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, error) {
	if session, ok := c.SessionByRealm(realm); ok {
		if session.Connected() {
			return session, nil
		}
		c.DeleteDeviceSession(realm)
	}

	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		return nil, err
	}

	cloudSession, err := xconn.ConnectCryptosign(ctx, CloudURI(), realm, authid, privKey)
	if err != nil {
		return nil, err
	}

	authenticator, err := auth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
	if err != nil {
		_ = cloudSession.Leave()
		return nil, err
	}
	config := &xconnwebrtc.ClientConfig{
		Realm:                    realm,
		ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
		TopicAnswererOnCandidate: TopicAnswererOnCandidate,
		TopicOffererOnCandidate:  TopicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            authenticator,
		Session:                  cloudSession,
		ICEServers:               []xconnwebrtc.ICEServer{{URLs: []string{StunServerURL}}},
	}

	webrtcSession, err := xconnwebrtc.ConnectWAMP(config)
	if err != nil {
		_ = cloudSession.Leave()
		return nil, fmt.Errorf("failed to establish P2P connection: %w", err)
	}

	_ = cloudSession.Leave()
	c.StoreDeviceSession(realm, webrtcSession)
	go c.reconnectLoop(webrtcSession, authid, privKey, realm) //nolint
	return webrtcSession, nil
}

func (c *ClientSessions) reconnectLoop(session *xconn.Session, authid, privateKey, realm string) {
	<-session.Done()
	retryDelay := 1 * time.Second
	maxDelay := 30 * time.Second
	for c.LoggedIn() {
		c.DeleteDeviceSession(realm)
		cloudSession, err := xconn.ConnectCryptosign(context.Background(), CloudURI(), realm, authid, privateKey)
		if err != nil {
			log.Printf("failed to connect cloud: %v", err)
			retryDelay *= 2
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			time.Sleep(retryDelay)
			continue
		}

		authenticator, err := auth.NewCryptoSignAuthenticator(authid, privateKey, map[string]any{})
		if err != nil {
			log.Printf("failed to create authenticator: %v", err)
			retryDelay *= 2
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			time.Sleep(retryDelay)
			continue
		}
		config := &xconnwebrtc.ClientConfig{
			Realm:                    realm,
			ProcedureWebRTCOffer:     ProcedureWebRTCOffer,
			TopicAnswererOnCandidate: TopicAnswererOnCandidate,
			TopicOffererOnCandidate:  TopicOffererOnCandidate,
			Serializer:               xconn.CBORSerializerSpec,
			Authenticator:            authenticator,
			Session:                  cloudSession,
			ICEServers: []xconnwebrtc.ICEServer{
				{URLs: []string{StunServerURL}},
			},
		}

		finalSession, err := xconnwebrtc.ConnectWAMP(config)
		if err != nil {
			log.Printf("failed to connect using webrtc: %v", err)
			retryDelay *= 2
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			time.Sleep(retryDelay)
			continue
		}
		_ = cloudSession.Leave()
		if sess, ok := c.SessionByRealm(realm); ok {
			if sess.Connected() {
				_ = finalSession.Leave()
				break
			}

			c.DeleteDeviceSession(realm)
		}
		c.StoreDeviceSession(realm, finalSession)
		<-finalSession.Done()
	}
}

func (c *ClientSessions) LoggedIn() bool {
	c.Lock()
	defer c.Unlock()
	return c.loggedIn
}

func (c *ClientSessions) Login() {
	c.Lock()
	defer c.Unlock()
	c.loggedIn = true
}

func (c *ClientSessions) Logout() {
	c.Lock()
	c.loggedIn = false
	deviceSessions := c.deviceSessionByRealm
	c.deviceSessionByRealm = make(map[string]*xconn.Session)
	c.Unlock()

	for _, session := range deviceSessions {
		_ = session.Leave()
	}
}
