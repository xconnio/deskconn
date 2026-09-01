package deskconn

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
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

func (p *ProxyCalls) DeleteAndClose(id uint64) {
	p.Lock()
	c, ok := p.calls[id]
	delete(p.calls, id)
	p.Unlock()
	if ok {
		c.closeChannel()
	}
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

	// webrtcSession is non-nil once this entry has upgraded to a direct
	// WebRTC P2P connection -- nil while it's still QUIC-only. It's the
	// only form that exposes OpenChannel, so features that need a raw data
	// channel (currently just VPN tunneling) key off this instead of
	// session's presence.
	webrtcSession *xconnwebrtc.WebRTCSession

	upgradeSubs []chan *xconn.Session
	sync.Mutex
}

// subscribeUpgrade registers interest in this session entry being replaced.
func (ds *deviceSession) subscribeUpgrade() <-chan *xconn.Session {
	ch := make(chan *xconn.Session, 1)
	ds.Lock()
	ds.upgradeSubs = append(ds.upgradeSubs, ch)
	ds.Unlock()
	return ch
}

// notifyReplaced delivers newSession to every subscriber registered via subscribeUpgrade and
// closes their channels. Called once, right after this entry stops being the realm's current
// one in ClientSessions.sessions.
func (ds *deviceSession) notifyReplaced(newSession *xconn.Session) {
	ds.Lock()
	subs := ds.upgradeSubs
	ds.upgradeSubs = nil
	ds.Unlock()
	for _, ch := range subs {
		ch <- newSession
		close(ch)
	}
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

// StoreDeviceSession installs session as realm's current device session. If it replaces an
// existing entry (a QUIC-to-P2P upgrade, or a reconnect after a network blip), every caller
// that subscribed to that old entry via EnsureDeviceSessionWithUpgrade is notified with the
// new session — not just whichever caller happened to trigger the replacement — so every
// long-lived proxied call sharing this device connection (e.g. a shell and its agent-forward
// call under "-A") can independently re-issue itself on the new session.
func (c *ClientSessions) StoreDeviceSession(realm string, session *xconn.Session,
	webrtcSession *xconnwebrtc.WebRTCSession, ctx context.Context, cancel context.CancelFunc) {
	c.Lock()
	old, hadOld := c.sessions[realm]
	if hadOld {
		old.cancel()
	}
	c.sessions[realm] = &deviceSession{
		session:       session,
		webrtcSession: webrtcSession,
		connectedAt:   time.Now(),
		ctx:           ctx,
		cancel:        cancel,
	}
	c.Unlock()

	if hadOld {
		old.notifyReplaced(session)
	}
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
	sess, _, err := c.ensureDeviceSession(ctx, realm, cfgDirectory, false)
	return sess, err
}

// EnsureDeviceSessionWithUpgrade is like EnsureDeviceSession but also subscribes to the
// realm's session entry being replaced — by the background QUIC-to-P2P upgrade this starts,
// or by a later reconnect after a network blip — and returns that subscription. Every
// concurrent caller gets its own subscription and is independently notified, which matters
// because "deskconn shell -A" runs two such callers (the shell call and the agent-forward
// call) at once; both need to learn about a swap, not just whichever happened to trigger it.
// Callers that use this must eventually stop reading it (the call ending is enough — the
// channel is buffered so a delivery to an abandoned subscriber never blocks), unlike
// EnsureDeviceSession, which is for callers with no such lifecycle to hang a subscription off.
func (c *ClientSessions) EnsureDeviceSessionWithUpgrade(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, <-chan *xconn.Session, error) {
	return c.ensureDeviceSession(ctx, realm, cfgDirectory, true)
}

func (c *ClientSessions) ensureDeviceSession(ctx context.Context, realm, cfgDirectory string,
	subscribe bool) (*xconn.Session, <-chan *xconn.Session, error) {
	c.Lock()
	delete(c.disconnected, realm)
	if ds, ok := c.sessions[realm]; ok {
		if ds.session.Connected() {
			var upgradeCh <-chan *xconn.Session
			if subscribe {
				upgradeCh = ds.subscribeUpgrade()
			}
			c.Unlock()
			return ds.session, upgradeCh, nil
		}
		ds.cancel()
		delete(c.sessions, realm)
		c.Unlock()
		return c.connectAndUpgrade(ctx, realm, cfgDirectory, ds, subscribe)
	}
	c.Unlock()

	return c.connectAndUpgrade(ctx, realm, cfgDirectory, nil, subscribe)
}

// connectAndUpgrade establishes a fresh QUIC session for realm and starts the background
// upgrade to P2P. staleEntry, if non-nil, is a disconnected entry the caller just evicted —
// its subscribers are notified with the fresh session (or, if even the QUIC connect fails,
// with nil, the same "give up" signal a failed P2P upgrade sends) once the outcome is known,
// so nobody who subscribed to it is left waiting on a channel that would otherwise never fire.
func (c *ClientSessions) connectAndUpgrade(ctx context.Context, realm, cfgDirectory string,
	staleEntry *deviceSession, subscribe bool) (*xconn.Session, <-chan *xconn.Session, error) {
	quicSess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		if staleEntry != nil {
			staleEntry.notifyReplaced(nil)
		}
		return nil, nil, err
	}

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	entry := &deviceSession{
		session:     quicSess.Session,
		connectedAt: time.Now(),
		ctx:         sessCtx,
		cancel:      cancel,
	}

	c.Lock()
	delete(c.disconnected, realm)
	if old, ok := c.sessions[realm]; ok {
		old.cancel()
	}
	c.sessions[realm] = entry
	c.Unlock()

	if staleEntry != nil {
		staleEntry.notifyReplaced(quicSess.Session)
	}

	var upgradeCh <-chan *xconn.Session
	if subscribe {
		upgradeCh = entry.subscribeUpgrade()
	}
	SafeGo(func() { c.upgradeToWebRTC(quicSess, realm, cfgDirectory) }) //nolint
	return quicSess.Session, upgradeCh, nil
}

// upgradeToWebRTC negotiates a WebRTC session using quicSess for signaling. On success, it atomically
// replaces the stored session, closes the QUIC connection, starts the reconnect loop.
func (c *ClientSessions) upgradeToWebRTC(quicSess *xconn.QUICSession, realm, cfgDirectory string) {
	authid, privKey, err := ReadCredentials(cfgDirectory)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return
	}

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	webrtcSess, err := connectWebrtcSession(quicSess.Session, realm, authid, privKey, cancel)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		cancel()
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return
	}

	if c.isDisconnected(realm) {
		cancel()
		_ = webrtcSess.Leave()
		return
	}

	c.StoreDeviceSession(realm, webrtcSess.Session, webrtcSess, sessCtx, cancel)
	log.Printf("p2p upgrade %s: upgraded to WebRTC", realm)

	SafeGo(func() { c.reconnectLoop(webrtcSess.Session, nil, realm, cfgDirectory) }) //nolint
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
		c.StoreDeviceSession(realm, newSess.Session, nil, sessCtx, cancel)
		log.Printf("reconnect %s: reconnected via QUIC", realm)
		SafeGo(func() { c.upgradeToWebRTC(newSess, realm, cfgDirectory) }) //nolint
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
