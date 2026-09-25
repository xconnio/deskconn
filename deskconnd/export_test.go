package deskconnd

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/xconnio/deskconn/common"
)

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
	shellID, migrationToken string, ptmx *os.File, startReader func(), err error) {
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
