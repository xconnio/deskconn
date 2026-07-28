package deskconn

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

type ProxyCall struct {
	progressChan  chan *xconn.Progress
	closeFunc     func()
	closed        bool
	streamID      uint64
	migrateBlobCh chan []byte // set once by ProxyShellMigrateHandler, read once during migration

	sync.Mutex
}

func newProxyCall() *ProxyCall {
	return &ProxyCall{
		progressChan:  make(chan *xconn.Progress, 32),
		migrateBlobCh: make(chan []byte, 1),
	}
}

// setMigrateBlob delivers the client-relayed encrypted migration token. Safe to call at
// most meaningfully once; later calls are dropped since the channel is already full.
func (pc *ProxyCall) setMigrateBlob(blob []byte) {
	select {
	case pc.migrateBlobCh <- blob:
	default:
	}
}

func (pc *ProxyCall) send(p *xconn.Progress) {
	pc.Lock()
	if pc.closed {
		pc.Unlock()
		return
	}
	ch := pc.progressChan
	pc.Unlock()
	ch <- p
}

func (pc *ProxyCall) switchChannel(newCh chan *xconn.Progress) {
	pc.Lock()
	pc.progressChan = newCh
	pc.Unlock()
}

func (pc *ProxyCall) setCloseFunc(f func()) {
	pc.Lock()
	pc.closeFunc = f
	pc.Unlock()
}

func (pc *ProxyCall) closeChannel() {
	pc.Lock()
	if pc.closed {
		pc.Unlock()
		return
	}
	pc.closed = true
	f := pc.closeFunc
	progressChan := pc.progressChan
	pc.Unlock()
	if f != nil {
		f()
	} else {
		close(progressChan)
	}
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

func (p *ProxyCalls) Fetch(id uint64) (*ProxyCall, bool) {
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

type ScreenshotConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

type Config struct {
	Devices    []Device         `yaml:"devices"`
	Printing   PrintingConfig   `yaml:"printing,omitempty"`
	Screenshot ScreenshotConfig `yaml:"screenshot,omitempty"`
}

type deviceSession struct {
	session     *xconn.Session
	connectedAt time.Time
	ctx         context.Context
	cancel      context.CancelFunc
}

type ClientSessions struct {
	sessions     map[string]*deviceSession
	disconnected map[string]struct{}
	loggedIn     bool
	sync.Mutex
}

func NewClientSessions() *ClientSessions {
	return &ClientSessions{
		sessions:     make(map[string]*deviceSession),
		disconnected: make(map[string]struct{}),
	}
}

func (c *ClientSessions) isDisconnected(realm string) bool {
	c.Lock()
	defer c.Unlock()
	_, ok := c.disconnected[realm]
	return ok
}

func (c *ClientSessions) SessionByRealm(realm string) (*xconn.Session, bool) {
	c.Lock()
	defer c.Unlock()
	session, ok := c.sessions[realm]
	if !ok {
		return nil, false
	}
	return session.session, true
}

func (c *ClientSessions) SessionContext(realm string) (context.Context, bool) {
	c.Lock()
	defer c.Unlock()
	session, ok := c.sessions[realm]
	if !ok {
		return nil, false
	}
	return session.ctx, true
}

func (c *ClientSessions) StoreDeviceSession(realm string, session *xconn.Session,
	ctx context.Context, cancel context.CancelFunc) {
	c.Lock()
	if old, ok := c.sessions[realm]; ok {
		old.cancel()
	}
	c.sessions[realm] = &deviceSession{
		session:     session,
		connectedAt: time.Now(),
		ctx:         ctx,
		cancel:      cancel,
	}
	c.Unlock()
}

func (c *ClientSessions) DeviceSessions() map[string]int64 {
	c.Lock()
	defer c.Unlock()
	result := make(map[string]int64, len(c.sessions))
	for realm, session := range c.sessions {
		result[realm] = session.connectedAt.Unix()
	}
	return result
}

func (c *ClientSessions) DeleteDeviceSession(realm string) {
	c.Lock()
	if session, ok := c.sessions[realm]; ok {
		session.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()
}

func (c *ClientSessions) Disconnect(realm string) {
	c.Lock()
	c.disconnected[realm] = struct{}{}
	session, ok := c.sessions[realm]
	if ok {
		session.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()

	if ok {
		_ = session.session.Leave()
	}
}

func (c *ClientSessions) DisconnectAll() {
	c.Lock()
	for realm := range c.sessions {
		c.disconnected[realm] = struct{}{}
	}
	sessions := c.sessions
	c.sessions = make(map[string]*deviceSession)
	c.Unlock()

	for _, entry := range sessions {
		entry.cancel()
		_ = entry.session.Leave()
	}
}

// EnsureDeviceSession returns the cached session if connected, otherwise connects via QUIC
// and starts a background upgrade to WebRTC P2P.
func (c *ClientSessions) EnsureDeviceSession(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, error) {
	sess, _, err := c.EnsureDeviceSessionWithUpgrade(ctx, realm, cfgDirectory)
	return sess, err
}

// EnsureDeviceSessionWithUpgrade is like EnsureDeviceSession but also returns a channel
// that receives the WebRTC session once the background upgrade completes. If the session
// is already a connected P2P session the channel is closed immediately.
func (c *ClientSessions) EnsureDeviceSessionWithUpgrade(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, <-chan *xconn.Session, error) {
	c.Lock()
	delete(c.disconnected, realm)
	if ds, ok := c.sessions[realm]; ok {
		if ds.session.Connected() {
			c.Unlock()
			ch := make(chan *xconn.Session)
			close(ch) // already connected (P2P or QUIC with upgrade in flight); caller need not migrate
			return ds.session, ch, nil
		}
		ds.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()

	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, nil, err
	}

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	c.Lock()
	delete(c.disconnected, realm)
	if old, ok := c.sessions[realm]; ok {
		old.cancel()
	}
	c.sessions[realm] = &deviceSession{
		session:     quicSess.Session,
		connectedAt: time.Now(),
		ctx:         sessCtx,
		cancel:      cancel,
	}
	c.Unlock()

	upgradeCh := make(chan *xconn.Session, 1)
	go func() { //nolint
		if sess := c.upgradeToWebRTC(quicSess, realm, cfgDirectory); sess != nil {
			upgradeCh <- sess
		}
		close(upgradeCh)
	}()
	return quicSess.Session, upgradeCh, nil
}

// upgradeToWebRTC negotiates a WebRTC session using quicSess for signalling. On success
// it atomically replaces the stored session, closes the QUIC connection, starts the
// reconnect loop, and returns the WebRTC session. Returns nil on failure.
func (c *ClientSessions) upgradeToWebRTC(quicSess *xconn.QUICSession, realm, cfgDirectory string) *xconn.Session {
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return nil
	}

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	webrtcSess, err := ConnectWebrtc(quicSess.Session, realm, authid, privKey, cancel)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		cancel()
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return nil
	}

	if c.isDisconnected(realm) {
		cancel()
		_ = webrtcSess.Leave()
		return nil
	}

	c.StoreDeviceSession(realm, webrtcSess, sessCtx, cancel)
	log.Printf("p2p upgrade %s: upgraded to WebRTC", realm)

	go c.reconnectLoop(webrtcSess, nil, realm, cfgDirectory) //nolint
	return webrtcSess
}

// reconnectLoop waits for session to disconnect then reconnects via QUIC and retries the P2P upgrade.
// conn is non-nil for QUIC sessions (closed on disconnect); nil for WebRTC sessions.
func (c *ClientSessions) reconnectLoop(session *xconn.Session, conn interface{ Close() error },
	realm, cfgDirectory string) {
	<-session.Done()
	if conn != nil {
		conn.Close()
	}

	if c.isDisconnected(realm) {
		return
	}

	retryDelay := 1 * time.Second
	maxDelay := 30 * time.Second
	for c.LoggedIn() && !c.isDisconnected(realm) {
		c.DeleteDeviceSession(realm)
		newSess, err := ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			log.Printf("failed to connect cloud: %v", err)
			retryDelay *= 2
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			time.Sleep(retryDelay)
			continue
		}
		sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
		c.StoreDeviceSession(realm, newSess.Session, sessCtx, cancel)
		log.Printf("reconnect %s: reconnected via QUIC", realm)
		go c.upgradeToWebRTC(newSess, realm, cfgDirectory) //nolint
		return
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
	sessions := c.sessions
	c.sessions = make(map[string]*deviceSession)
	c.Unlock()

	for _, session := range sessions {
		session.cancel()
		_ = session.session.Leave()
	}
}
