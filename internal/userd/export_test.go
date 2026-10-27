package userd

import (
	"net"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
)

type App = app

// The tests serve the agents' procedures whether or not they're installed.
func init() { agentInstalled = func(string) bool { return true } }

// Capabilities is capabilities for tests, already registering on its session.
type Capabilities struct{ c *capabilities }

func NewCapabilities(own []*App, cfgDirectory string, session *xconn.Session) (*Capabilities, error) {
	c := newCapabilities(own, cfgDirectory)
	return &Capabilities{c}, c.register(session)
}

func (c *Capabilities) Enabled(id string) bool        { return c.c.enabled(id) }
func (c *Capabilities) List() xconn.InvocationHandler { return c.c.handleList }
func (c *Capabilities) Set() xconn.InvocationHandler  { return c.c.handleSet }
func (c *Capabilities) AddExternal(sessionID uint64, apps []*App) error {
	return c.c.addExternal(sessionID, apps)
}
func (c *Capabilities) RemoveExternal(sessionID uint64) { c.c.removeExternal(sessionID) }

// Test-only access to unexported stream handlers and shell session state, for the
// package's external tests.

func (d *Deskconn) HandleQUICLogsStream(stream net.Conn) { d.handleQUICLogsStream(stream) }
func (d *Deskconn) HandleQUICPortForwardStream(stream net.Conn) {
	d.handleQUICPortForwardStream(stream)
}
func (d *Deskconn) HandleQUICPortReverseStream(stream net.Conn) {
	d.handleQUICPortReverseStream(stream)
}
func (d *Deskconn) HandleQUICAgentForwardStream(stream net.Conn) {
	d.handleQUICAgentForwardStream(stream)
}

// NewAgentForwardDeskconn returns a Deskconn with only agent forwarding set up.
func NewAgentForwardDeskconn() *Deskconn {
	return &Deskconn{agentForwardSessions: newAgentForwardSessions()}
}

// AgentSocketPath reports the forwarded agent socket registered for authID.
func (d *Deskconn) AgentSocketPath(authID string) (string, bool) {
	return d.agentForwardSessions.socketPathByAuthID(authID)
}

// NewShellDeskconn returns a Deskconn with only the shell feature set up.
func NewShellDeskconn() *Deskconn {
	return &Deskconn{shellSession: newInteractiveShellSession()}
}

type InteractiveShellSession = interactiveShellSession

var NewInteractiveShellSession = newInteractiveShellSession

func (p *interactiveShellSession) BeginShellSession(ctrl common.ShellControlMsg, transport shellTransport) (
	shellID, migrationToken string, ptmx pty.Pty, startReader func(), err error) {
	return p.beginShellSession(ctrl, transport)
}

func (p *interactiveShellSession) CleanupShellID(shellID string) { p.cleanupShellID(shellID) }

func (p *interactiveShellSession) EndShellInput(shellID string, transport shellTransport) {
	p.endShellInput(shellID, transport)
}

func (p *interactiveShellSession) HasPTY(shellID string) bool {
	p.Lock()
	defer p.Unlock()
	_, ok := p.ptmx[shellID]
	return ok
}

func (p *interactiveShellSession) HasSession(shellID string) bool {
	p.Lock()
	defer p.Unlock()
	_, ok := p.sessions[shellID]
	return ok
}

func (p *interactiveShellSession) PID(shellID string) int {
	p.Lock()
	defer p.Unlock()
	return p.pids[shellID]
}

func (p *interactiveShellSession) HasMigrationToken(shellID string) bool {
	p.Lock()
	defer p.Unlock()
	_, ok := p.migrationTokens[shellID]
	return ok
}

// SessionTransport returns the transport currently attached to shellID's session.
func (p *interactiveShellSession) SessionTransport(shellID string) any {
	p.Lock()
	ps := p.sessions[shellID]
	p.Unlock()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.transport
}

// ExpireMigrationToken backdates shellID's pending migration token past its TTL.
func (p *interactiveShellSession) ExpireMigrationToken(shellID string) {
	p.Lock()
	defer p.Unlock()
	token := p.migrationTokens[shellID]
	token.issuedAt = time.Now().Add(-migrationTokenTTL - time.Second)
	p.migrationTokens[shellID] = token
}

// FakeShellTransport is a shellTransport that just records what it's asked to
// write, for unit-testing shell sessions without a real QUIC stream or WebRTC
// channel. Guarded by mu since writeOutput runs on the PTY output-reader
// goroutine while tests read what was written from the main test goroutine.
type FakeShellTransport struct {
	mu      sync.Mutex
	written [][]byte
	closed  bool
}

func (t *FakeShellTransport) writeOutput(plaintext []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.written = append(t.written, append([]byte(nil), plaintext...))
	return nil
}

func (t *FakeShellTransport) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// Written returns a copy of every chunk written so far.
func (t *FakeShellTransport) Written() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]byte(nil), t.written...)
}

// QUICConn exposes quicConn's lifecycle; closeConn stands in for the connection.
type QUICConn = quicConn

func NewQUICConn(closeConn func() error) *QUICConn { return &quicConn{closeConn: closeConn} }

func (q *quicConn) Acquire() bool { return q.acquire() }
func (q *quicConn) Release()      { q.release() }
func (q *quicConn) Retire()       { q.retire() }
func (q *quicConn) Close()        { q.closeIf(true) }

// RetireQUIC retires realm's QUIC connection, as a P2P upgrade does.
func (c *ClientSessions) RetireQUIC(realm string) {
	c.Lock()
	quic := c.sessions[realm].quic
	c.Unlock()
	quic.retire()
}

// NewMountFS returns a mount server's request handler, as one caller session, and a
// func reporting how many files it has open.
func NewMountFS() (func(payload []byte) ([]byte, error), func() int) {
	h := newMountHandles()
	call := func(payload []byte) ([]byte, error) { return h.call(1, payload) }
	open := func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.files)
	}
	return call, open
}
