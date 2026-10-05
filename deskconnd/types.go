package deskconnd

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
	xconnauth "github.com/xconnio/xconn-go/auth"
	xconnwebrtc "github.com/xconnio/xconn-webrtc-go"
)

// quicConn is a device's QUIC connection plus a count of the operations running on it.
// Once retired (its session upgraded to P2P) it closes as soon as that count is zero.
type quicConn struct {
	*common.DeviceConn
	closeConn func() error

	mu      sync.Mutex
	users   int
	retired bool
	closed  bool
}

func newQUICConn(conn *common.DeviceConn) *quicConn {
	return &quicConn{DeviceConn: conn, closeConn: conn.Connection().Close}
}

// acquire reports whether the connection can still be used, and counts the caller as a
// user until it calls release.
func (q *quicConn) acquire() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.users++
	return true
}

func (q *quicConn) release() {
	q.mu.Lock()
	q.users--
	q.mu.Unlock()
	q.closeIf(false)
}

func (q *quicConn) retire() {
	q.mu.Lock()
	q.retired = true
	q.mu.Unlock()
	q.closeIf(false)
}

// closeIf closes the connection if force is set, or if it's retired and unused.
func (q *quicConn) closeIf(force bool) {
	q.mu.Lock()
	closeNow := !q.closed && (force || (q.retired && q.users == 0))
	if closeNow {
		q.closed = true
	}
	q.mu.Unlock()
	if closeNow {
		_ = q.closeConn()
	}
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

	// quic is the connection the entry was established on. After a P2P upgrade it's
	// retired: only operations that started on it still use it.
	quic *quicConn

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

// closeTransports closes a dropped entry's connections: its QUIC connection, unless its
// successor keeps the same one, and its PeerConnection (leaving the session doesn't).
func (ds *deviceSession) closeTransports(keepQUIC *quicConn) {
	if ds.quic != nil && ds.quic != keepQUIC {
		ds.quic.closeIf(true)
	}
	if ds.webrtcSession != nil {
		_ = ds.webrtcSession.Connection().Close()
	}
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
// long-lived proxied call sharing this device connection can independently re-issue itself
// on the new session.
func (c *ClientSessions) StoreDeviceSession(realm string, session *xconn.Session,
	webrtcSession *xconnwebrtc.WebRTCSession, quic *common.DeviceConn, ctx context.Context,
	cancel context.CancelFunc) {
	var conn *quicConn
	if quic != nil {
		conn = newQUICConn(quic)
	}
	c.storeDeviceSession(realm, session, webrtcSession, conn, ctx, cancel)
}

func (c *ClientSessions) storeDeviceSession(realm string, session *xconn.Session,
	webrtcSession *xconnwebrtc.WebRTCSession, quic *quicConn, ctx context.Context,
	cancel context.CancelFunc) {
	c.Lock()
	old, hadOld := c.sessions[realm]
	if hadOld {
		old.cancel()
	}
	c.sessions[realm] = &deviceSession{
		session:       session,
		webrtcSession: webrtcSession,
		quic:          quic,
		connectedAt:   time.Now(),
		ctx:           ctx,
		cancel:        cancel,
	}
	c.Unlock()

	if hadOld {
		old.closeTransports(quic)
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
	session, ok := c.sessions[realm]
	if ok {
		session.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()

	if ok {
		session.closeTransports(nil)
	}
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
		session.closeTransports(nil)
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
		entry.closeTransports(nil)
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
// concurrent caller gets its own subscription and is independently notified, for a proxied
// call that needs to re-issue itself on the new session after a transport swap rather than
// just learning its own call ended. Callers that use this must eventually stop reading it
// (the call ending is enough — the channel is buffered so a delivery to an abandoned
// subscriber never blocks), unlike EnsureDeviceSession, which is for callers with no such
// lifecycle to hang a subscription off.
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
	deviceConn, err := common.ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		if staleEntry != nil {
			staleEntry.notifyReplaced(nil)
		}
		return nil, nil, err
	}
	quicSess := newQUICConn(deviceConn)

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	entry := &deviceSession{
		session:     quicSess.Session,
		quic:        quicSess,
		connectedAt: time.Now(),
		ctx:         sessCtx,
		cancel:      cancel,
	}

	c.Lock()
	delete(c.disconnected, realm)
	old, hadOld := c.sessions[realm]
	if hadOld {
		old.cancel()
	}
	c.sessions[realm] = entry
	c.Unlock()

	if hadOld {
		old.closeTransports(quicSess)
	}

	if staleEntry != nil {
		staleEntry.notifyReplaced(quicSess.Session)
	}

	var upgradeCh <-chan *xconn.Session
	if subscribe {
		upgradeCh = entry.subscribeUpgrade()
	}
	common.SafeGo(func() { c.upgradeToWebRTC(quicSess, realm, cfgDirectory) }) //nolint
	return quicSess.Session, upgradeCh, nil
}

// upgradeToWebRTC negotiates a WebRTC session using quicSess for signaling. On success, it atomically
// replaces the stored session, retires the QUIC connection, starts the reconnect loop.
func (c *ClientSessions) upgradeToWebRTC(quicSess *quicConn, realm, cfgDirectory string) {
	authid, privKey, err := common.ReadCredentials(cfgDirectory)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return
	}
	authenticator, err := xconnauth.NewCryptoSignAuthenticator(authid, privKey, map[string]any{})
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return
	}

	sessCtx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck
	webrtcSess, err := common.ConnectWebrtcSession(quicSess.Session, realm, authenticator, cancel)
	if err != nil {
		log.Printf("p2p upgrade %s: %v", realm, err)
		cancel()
		c.reconnectLoop(quicSess.Session, quicSess.Connection(), realm, cfgDirectory)
		return
	}

	if c.isDisconnected(realm) {
		cancel()
		_ = webrtcSess.Leave()
		_ = webrtcSess.Connection().Close()
		return
	}

	c.storeDeviceSession(realm, webrtcSess.Session, webrtcSess, quicSess, sessCtx, cancel)
	quicSess.retire()
	log.Printf("p2p upgrade %s: upgraded to WebRTC", realm)

	common.SafeGo(func() { c.reconnectLoop(webrtcSess.Session, nil, realm, cfgDirectory) }) //nolint
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
		deviceConn, err := common.ConnectDeviceRealmQUIC(context.Background(), realm, cfgDirectory)
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
		newSess := newQUICConn(deviceConn)
		c.storeDeviceSession(realm, newSess.Session, nil, newSess, sessCtx, cancel)
		log.Printf("reconnect %s: reconnected via QUIC", realm)
		common.SafeGo(func() { c.upgradeToWebRTC(newSess, realm, cfgDirectory) }) //nolint
		return
	}
}

// deviceTransports returns what raw streams to realm's device can be opened on, connecting
// first if needed: quic (retired, possibly closed, once rtc is set) and rtc.
func (c *ClientSessions) deviceTransports(ctx context.Context, realm,
	cfgDirectory string) (quic *quicConn, rtc *xconnwebrtc.WebRTCSession, err error) {
	if _, err := c.EnsureDeviceSession(ctx, realm, cfgDirectory); err != nil {
		return nil, nil, err
	}

	c.Lock()
	defer c.Unlock()
	ds, ok := c.sessions[realm]
	if !ok {
		return nil, nil, fmt.Errorf("connection to %s was lost", realm)
	}
	return ds.quic, ds.webrtcSession, nil
}

// holdSession keeps the QUIC connection session runs on, if it does, open until the returned
// func is called, so a call in flight when the P2P upgrade lands isn't cut off.
func (c *ClientSessions) holdSession(realm string, session *xconn.Session) func() {
	c.Lock()
	defer c.Unlock()
	ds, ok := c.sessions[realm]
	if ok && ds.session == session && ds.quic != nil && ds.webrtcSession == nil && ds.quic.acquire() {
		return ds.quic.release
	}
	return func() {}
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
		session.closeTransports(nil)
	}
}
