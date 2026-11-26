package userd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
)

// machineSession is the daemon's connection to a machine. XConn sends each new call on session
// over a direct WebRTC session once the machine has one, and otherwise over the Hub, which
// stays connected; only the Hub session ending ends the entry.
type machineSession struct {
	session     *xconn.Session
	connectedAt time.Time
	ctx         context.Context
	cancel      context.CancelFunc

	// conn is session's connection, closed with the entry; nil for a session the daemon was
	// given rather than made.
	conn *connectivity.MachineConn
}

// closeTransport closes a dropped entry's connection, unless its successor keeps it.
func (ds *machineSession) closeTransport(keep *connectivity.MachineConn) {
	if ds.conn != nil && ds.conn != keep {
		_ = ds.conn.Connection().Close()
	}
}

type ClientSessions struct {
	sessions     map[string]*machineSession
	disconnected map[string]struct{}
	connecting   map[string]chan struct{} // by realm, closed once the connection being made is made or fails
	loggedIn     bool

	// keys are the proxy handlers' keys for each machine session.
	keys *proxyKeyCache

	sync.Mutex
}

func NewClientSessions() *ClientSessions {
	return &ClientSessions{
		sessions:     make(map[string]*machineSession),
		disconnected: make(map[string]struct{}),
		connecting:   make(map[string]chan struct{}),
		keys:         newProxyKeyCache(),
	}
}

// ErrMachineOffline is the error connecting to a machine that isn't online fails with.
var ErrMachineOffline = errors.New("the machine is offline")

// machineAnswersTimeout bounds waiting for a machine to answer.
const machineAnswersTimeout = 10 * time.Second

// machineAnswers pings session's machine, failing with ErrMachineOffline if no machine is
// there to answer: the router takes a session for a machine's realm whether or not the
// machine is online.
func machineAnswers(ctx context.Context, session *xconn.Session) error {
	ctx, cancel := context.WithTimeout(ctx, machineAnswersTimeout)
	defer cancel()
	err := session.Call(common.ProcedurePing).ExecuteContext(ctx).Err
	if err != nil && strings.Contains(err.Error(), common.ErrNoSuchProcedure) {
		return ErrMachineOffline
	}
	return err
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

// StoreMachineSession installs session, on conn, as realm's current machine session.
func (c *ClientSessions) StoreMachineSession(realm string, session *xconn.Session, conn *connectivity.MachineConn,
	ctx context.Context, cancel context.CancelFunc) {
	c.Lock()
	old, hadOld := c.sessions[realm]
	if hadOld {
		old.cancel()
	}
	c.sessions[realm] = &machineSession{
		session:     session,
		conn:        conn,
		connectedAt: time.Now(),
		ctx:         ctx,
		cancel:      cancel,
	}
	c.Unlock()

	if hadOld {
		old.closeTransport(conn)
	}
}

func (c *ClientSessions) MachineSessions() map[string]int64 {
	c.Lock()
	defer c.Unlock()
	result := make(map[string]int64, len(c.sessions))
	for realm, session := range c.sessions {
		result[realm] = session.connectedAt.Unix()
	}
	return result
}

func (c *ClientSessions) DeleteMachineSession(realm string) {
	c.Lock()
	session, ok := c.sessions[realm]
	if ok {
		session.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()

	if ok {
		session.closeTransport(nil)
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
		session.closeTransport(nil)
	}
}

func (c *ClientSessions) DisconnectAll() {
	c.Lock()
	for realm := range c.sessions {
		c.disconnected[realm] = struct{}{}
	}
	sessions := c.sessions
	c.sessions = make(map[string]*machineSession)
	c.Unlock()

	for _, entry := range sessions {
		entry.cancel()
		_ = entry.session.Leave()
		entry.closeTransport(nil)
	}
}

// EnsureMachineSession returns realm's machine session, connecting through the Hub if there's
// none; XConn negotiates a direct path in the background, without holding up calls.
func (c *ClientSessions) EnsureMachineSession(ctx context.Context, realm,
	cfgDirectory string) (*xconn.Session, error) {
	c.Lock()
	delete(c.disconnected, realm)
	// Callers at once wait for one connection: each making its own, the last would replace
	// the others under the calls on them.
	for {
		if ds, ok := c.sessions[realm]; ok && ds.session.Connected() {
			c.Unlock()
			return ds.session, nil
		}
		connecting, ok := c.connecting[realm]
		if !ok {
			break
		}
		c.Unlock()
		select {
		case <-connecting:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.Lock()
	}
	connecting := make(chan struct{})
	c.connecting[realm] = connecting
	if stale := c.sessions[realm]; stale != nil {
		stale.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()

	defer func() {
		c.Lock()
		delete(c.connecting, realm)
		close(connecting)
		c.Unlock()
	}()
	conn, err := c.connectMachine(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	c.Lock()
	delete(c.disconnected, realm)
	c.Unlock()
	// The connection outlives the request that made it.
	c.install(realm, conn, cfgDirectory) //nolint:contextcheck
	return conn.Session, nil
}

// install makes conn realm's machine session and reconnects once its Hub session ends.
func (c *ClientSessions) install(realm string, conn *connectivity.MachineConn, cfgDirectory string) {
	sessCtx, cancel := context.WithCancel(context.Background())
	c.StoreMachineSession(realm, conn.Session, conn, sessCtx, cancel)
	common.SafeGo(func() { c.reconnectLoop(conn, realm, cfgDirectory) }) //nolint
}

// reconnectLoop waits for conn's Hub session to end, then reconnects: a dropped direct path
// doesn't end it, since new calls go through the Hub meanwhile.
func (c *ClientSessions) reconnectLoop(conn *connectivity.MachineConn, realm, cfgDirectory string) {
	<-conn.Done()
	_ = conn.Connection().Close()

	// The connection is gone whatever happens next, unless one that replaced it is current.
	if !c.forget(realm, conn.Session) || c.isDisconnected(realm) {
		return
	}

	retryDelay := 1 * time.Second
	maxDelay := 30 * time.Second
	// A standalone machine doesn't need the account: signing out leaves it connected.
	for (c.LoggedIn() || common.IsStandaloneMachineRealm(realm)) && !c.isDisconnected(realm) {
		c.DeleteMachineSession(realm)
		machineConn, err := c.connectMachine(context.Background(), realm, cfgDirectory)
		if err != nil {
			log.Printf("reconnect %s: %v", realm, err)
			retryDelay *= 2
			if retryDelay > maxDelay {
				retryDelay = maxDelay
			}
			time.Sleep(retryDelay)
			continue
		}
		c.install(realm, machineConn, cfgDirectory)
		log.Printf("reconnect %s: reconnected via QUIC", realm)
		return
	}
}

// forget removes realm's entry if it's session's, reporting whether realm is left without
// one: false if another session replaced it.
func (c *ClientSessions) forget(realm string, session *xconn.Session) bool {
	c.Lock()
	entry, ok := c.sessions[realm]
	if ok && entry.session != session {
		c.Unlock()
		return false
	}
	if ok {
		entry.cancel()
		delete(c.sessions, realm)
	}
	c.Unlock()
	if ok {
		entry.closeTransport(nil)
	}
	return true
}

// connectMachine connects to realm's machine through the router, or directly to a standalone
// machine saved in config.yml, failing with ErrMachineOffline unless the machine answers on
// the connection.
func (c *ClientSessions) connectMachine(ctx context.Context, realm, cfgDirectory string) (
	*connectivity.MachineConn, error) {
	conn, err := connectMachineQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	if err := machineAnswers(ctx, conn.Session); err != nil {
		_ = conn.Close()
		_ = conn.Connection().Close()
		return nil, err
	}
	return conn, nil
}

// connectMachineQUIC is connectivity.ConnectMachineRealmManaged with, for a standalone machine,
// its address and login read afresh from config.yml, which this process learns them from.
func connectMachineQUIC(ctx context.Context, realm, cfgDirectory string) (*connectivity.MachineConn, error) {
	if common.IsStandaloneMachineRealm(realm) {
		if err := common.LoadStandaloneTarget(cfgDirectory, realm); err != nil {
			return nil, err
		}
	}
	return connectivity.ConnectMachineRealmManaged(ctx, realm, cfgDirectory)
}

// directPath reports whether new calls to realm's machine go over a direct WebRTC session,
// connecting first if needed.
func (c *ClientSessions) directPath(ctx context.Context, realm, cfgDirectory string) (bool, error) {
	session, err := c.EnsureMachineSession(ctx, realm, cfgDirectory)
	if err != nil {
		return false, err
	}
	return session.Path() == xconn.PathDirect, nil
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

// Logout closes the connections to the account's machines; those to standalone machines,
// which don't log in with the account, stay.
func (c *ClientSessions) Logout() {
	c.Lock()
	c.loggedIn = false
	sessions := make(map[string]*machineSession)
	for realm, session := range c.sessions {
		if !common.IsStandaloneMachineRealm(realm) {
			sessions[realm] = session
			delete(c.sessions, realm)
		}
	}
	c.Unlock()

	for _, session := range sessions {
		session.cancel()
		_ = session.session.Leave()
		session.closeTransport(nil)
	}
}
