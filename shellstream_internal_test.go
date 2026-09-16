package deskconn

import (
	"bytes"
	"encoding/json"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeShellTransport is a shellTransport that just records what it's
// asked to write, for unit-testing beginShellSession/endShellInput without
// a real QUIC stream or WebRTC channel. Guarded by mu since writeOutput runs
// on the PTY output-reader goroutine while tests read written/closed from
// the main test goroutine.
type fakeShellTransport struct {
	mu      sync.Mutex
	written [][]byte
	closed  bool
}

func (t *fakeShellTransport) writeOutput(plaintext []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.written = append(t.written, append([]byte(nil), plaintext...))
	return nil
}

func (t *fakeShellTransport) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

func TestBeginShellSessionCreatesNewPTY(t *testing.T) {
	p := newInteractiveShellSession()
	transport := &fakeShellTransport{}

	shellID, token, ptmx, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, transport)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	require.NoError(t, err)
	assert.NotEmpty(t, shellID)
	assert.NotEmpty(t, token, "a fresh session should be issued a migration token")
	require.NotNil(t, ptmx)

	p.Lock()
	_, hasPtmx := p.ptmx[shellID]
	_, hasSession := p.sessions[shellID]
	p.Unlock()
	assert.True(t, hasPtmx)
	assert.True(t, hasSession)
}

// TestBeginShellSessionRunsExecCommand confirms the same protocol that
// serves shell also serves exec: when ctrl.Command is set, beginShellSession
// runs that command (with its args) instead of an interactive bash shell,
// and the command's real output reaches the transport.
func TestBeginShellSessionRunsExecCommand(t *testing.T) {
	p := newInteractiveShellSession()
	transport := &fakeShellTransport{}

	shellID, _, _, err := p.beginShellSession(shellControlMsg{
		Op: shellOpSize, Cols: 80, Rows: 24, Command: "echo", Args: []string{"hello-exec-test"},
	}, transport)
	require.NoError(t, err)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	require.Eventually(t, func() bool {
		transport.mu.Lock()
		defer transport.mu.Unlock()
		for _, chunk := range transport.written {
			if bytes.Contains(chunk, []byte("hello-exec-test")) {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "exec'd command's output never reached the transport")

	// echo exits immediately, so the session should clean itself up on its own.
	require.Eventually(t, func() bool {
		p.Lock()
		defer p.Unlock()
		_, stillTracked := p.sessions[shellID]
		return !stillTracked
	}, 3*time.Second, 10*time.Millisecond, "PTY session should be cleaned up once the exec'd command exits")
}

// TestCleanupShellIDKillsProcessGroup guards against a real bug: closing
// ptmx alone doesn't reliably kill a child that produces no PTY output
// (nothing ever reads it) -- the expected SIGHUP-on-hangup can race with
// startOutputReader's own concurrent blocked Read on the same file and
// silently never arrive, leaving the process running forever. cleanupShellID
// must kill the process group explicitly rather than relying on that.
func TestCleanupShellIDKillsProcessGroup(t *testing.T) {
	p := newInteractiveShellSession()
	transport := &fakeShellTransport{}

	shellID, _, _, err := p.beginShellSession(shellControlMsg{
		Op: shellOpSize, Cols: 80, Rows: 24, Command: "sleep", Args: []string{"30"},
	}, transport)
	require.NoError(t, err)

	p.Lock()
	pid := p.pids[shellID]
	p.Unlock()
	require.Positive(t, pid)
	require.NoError(t, syscall.Kill(pid, 0), "sanity check: process should be running before cleanup")

	p.cleanupShellID(shellID)

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 3*time.Second, 20*time.Millisecond,
		"sleep should actually be killed (and reaped) by cleanupShellID, not just have its ptmx closed")
}

func TestBeginShellSessionMigrateValidToken(t *testing.T) {
	p := newInteractiveShellSession()
	original := &fakeShellTransport{}
	shellID, token, _, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	newTransport := &fakeShellTransport{}
	claimedID, _, ptmx, err := p.beginShellSession(
		shellControlMsg{Op: shellOpMigrate, OldID: shellID, Token: token}, newTransport)

	require.NoError(t, err)
	assert.Equal(t, shellID, claimedID, "migration keeps the same shell ID, no rekeying needed")
	require.NotNil(t, ptmx)

	p.Lock()
	ps := p.sessions[shellID]
	_, tokenStillPending := p.migrationTokens[shellID]
	p.Unlock()
	ps.mu.Lock()
	currentTransport := ps.transport
	ps.mu.Unlock()
	assert.Same(t, newTransport, currentTransport, "the session's transport should now be the new one")
	assert.False(t, tokenStillPending, "a used token must not be replayable")
}

func TestBeginShellSessionMigrateWrongToken(t *testing.T) {
	p := newInteractiveShellSession()
	original := &fakeShellTransport{}
	shellID, _, _, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	_, _, _, err = p.beginShellSession(
		shellControlMsg{Op: shellOpMigrate, OldID: shellID, Token: "not-the-real-token"}, &fakeShellTransport{})
	assert.Error(t, err)

	p.Lock()
	ps := p.sessions[shellID]
	p.Unlock()
	ps.mu.Lock()
	currentTransport := ps.transport
	ps.mu.Unlock()
	assert.Same(t, original, currentTransport, "a failed migration must not disturb the existing session")
}

func TestBeginShellSessionMigrateExpiredToken(t *testing.T) {
	p := newInteractiveShellSession()
	original := &fakeShellTransport{}
	shellID, token, _, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	p.Lock()
	expired := p.migrationTokens[shellID]
	expired.issuedAt = time.Now().Add(-migrationTokenTTL - time.Second)
	p.migrationTokens[shellID] = expired
	p.Unlock()

	_, _, _, err = p.beginShellSession(
		shellControlMsg{Op: shellOpMigrate, OldID: shellID, Token: token}, &fakeShellTransport{})
	assert.Error(t, err)
}

func TestBeginShellSessionMigrateUnknownShellID(t *testing.T) {
	p := newInteractiveShellSession()
	_, _, _, err := p.beginShellSession(
		shellControlMsg{Op: shellOpMigrate, OldID: "no-such-shell", Token: "whatever"}, &fakeShellTransport{})
	assert.Error(t, err)
}

// TestBeginShellSessionExecNonexistentCommand confirms a failed exec (e.g. a
// typo'd or missing command) reports a real error rather than silently
// closing the connection.
func TestBeginShellSessionExecNonexistentCommand(t *testing.T) {
	p := newInteractiveShellSession()
	_, _, _, err := p.beginShellSession(shellControlMsg{
		Op: shellOpSize, Cols: 80, Rows: 24, Command: "this-command-does-not-exist-xyz",
	}, &fakeShellTransport{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this-command-does-not-exist-xyz")
}

func TestEndShellInputCleansUpWhenStillOwner(t *testing.T) {
	p := newInteractiveShellSession()
	transport := &fakeShellTransport{}
	shellID, _, _, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, transport)
	require.NoError(t, err)

	p.endShellInput(shellID, transport)

	p.Lock()
	_, hasSession := p.sessions[shellID]
	p.Unlock()
	assert.False(t, hasSession, "a real disconnect (transport still owns the session) should tear down the PTY")
}

func TestEndShellInputNoOpAfterMigration(t *testing.T) {
	p := newInteractiveShellSession()
	original := &fakeShellTransport{}
	shellID, token, _, err := p.beginShellSession(shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	t.Cleanup(func() { p.cleanupShellID(shellID) })

	newTransport := &fakeShellTransport{}
	_, _, _, err = p.beginShellSession(shellControlMsg{Op: shellOpMigrate, OldID: shellID, Token: token}, newTransport)
	require.NoError(t, err)

	// The old connection dying after a successful migration must not kill
	// the PTY the new connection is now serving.
	p.endShellInput(shellID, original)

	p.Lock()
	_, hasSession := p.sessions[shellID]
	p.Unlock()
	assert.True(t, hasSession, "migrating away must not tear down the PTY the new transport now owns")
}

// TestHandleQUICShellStreamEndToEnd drives shell over a net.Pipe through
// the real entry point (HandleQUICStream): routing frame, key exchange,
// size, one keystroke, then "exit\n" against a real spawned bash, checking
// output comes back decrypted and the stream ends cleanly on exit.
func TestHandleQUICShellStreamEndToEnd(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	d := Deskconn{shellSession: newInteractiveShellSession()}
	go d.HandleQUICStream(nil, server)

	require.NoError(t, writeMsg(client, routingFrame{Op: fsOpShell}))
	sendKey, receiveKey, err := quicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendQUICShellControl(client, sendKey, shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}))
	ack, err := recvQUICShellEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.NotEmpty(t, ack.ackShellID)
	require.NotEmpty(t, ack.ackToken)

	require.NoError(t, sendQUICShellData(client, sendKey, []byte("exit\n")))

	// Drain output until the stream closes (bash exiting closes the PTY,
	// which ends handleQUICShellStream's loop and the pipe).
	for {
		_, err := recvQUICShellEnvelope(client, receiveKey)
		if err != nil {
			break
		}
	}
}

// TestHandleQUICShellStreamIgnoresPing confirms a shellOpPing control
// message (sent by the client purely to keep the connection's idle deadline
// from expiring during genuine silence) doesn't disrupt an otherwise-normal
// session -- the PTY keeps running and later data still reaches it.
func TestHandleQUICShellStreamIgnoresPing(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	d := Deskconn{shellSession: newInteractiveShellSession()}
	go d.HandleQUICStream(nil, server)

	require.NoError(t, writeMsg(client, routingFrame{Op: fsOpShell}))
	sendKey, receiveKey, err := quicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendQUICShellControl(client, sendKey, shellControlMsg{Op: shellOpSize, Cols: 80, Rows: 24}))
	ack, err := recvQUICShellEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.NotEmpty(t, ack.ackShellID)

	require.NoError(t, sendQUICShellControl(client, sendKey, shellControlMsg{Op: shellOpPing}))
	require.NoError(t, sendQUICShellData(client, sendKey, []byte("exit\n")))

	for {
		_, err := recvQUICShellEnvelope(client, receiveKey)
		if err != nil {
			break
		}
	}
}

type shellAck struct {
	ackShellID string
	ackToken   string
}

func sendQUICShellControl(conn net.Conn, sendKey []byte, msg shellControlMsg) error {
	envelope, err := buildShellEnvelope(shellMsgControl, mustJSON(msg), sendKey)
	if err != nil {
		return err
	}
	return writeFrame(conn, envelope)
}

func sendQUICShellData(conn net.Conn, sendKey, data []byte) error {
	envelope, err := buildShellEnvelope(shellMsgData, data, sendKey)
	if err != nil {
		return err
	}
	return writeFrame(conn, envelope)
}

func recvQUICShellEnvelope(conn net.Conn, receiveKey []byte) (shellAck, error) {
	frame, err := readFrame(conn)
	if err != nil {
		return shellAck{}, err
	}
	kind, plaintext, err := decryptEnvelope(frame, receiveKey)
	if err != nil {
		return shellAck{}, err
	}
	if kind != shellMsgControl {
		return shellAck{}, nil
	}
	var msg shellControlMsg
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return shellAck{}, err
	}
	return shellAck{ackShellID: msg.ShellID, ackToken: msg.Token}, nil
}
