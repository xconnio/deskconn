package deskconn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

func (c *ClientSessions) DeleteDeviceSession(realm string) {
	c.Lock()
	defer c.Unlock()
	delete(c.deviceSessionByRealm, realm)
}

func (c *ClientSessions) EnsureDeviceSession(ctx context.Context, realm, cfgDirectory string) (*xconn.Session, error) {
	if session, ok := c.SessionByRealm(realm); ok {
		if session.Connected() {
			return session, nil
		}

		c.DeleteDeviceSession(realm)
	}

	credentialsStr, err := os.ReadFile(filepath.Join(cfgDirectory, "id_ed25519"))
	if err != nil {
		return nil, fmt.Errorf("kindly login first: %w", err)
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
		return session, nil
	}

	_ = session.Leave()
	c.StoreDeviceSession(realm, finalSession)

	go c.reconnectLoop(finalSession, authid, privKey, realm) //nolint
	return finalSession, nil
}

func (c *ClientSessions) reconnectLoop(session *xconn.Session, authid, privateKey, realm string) {
	<-session.Done()
	retryDelay := 1 * time.Second
	maxDelay := 30 * time.Second
	for {
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
