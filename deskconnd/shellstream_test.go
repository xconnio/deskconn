package deskconnd_test

import (
	"bytes"
	"encoding/json"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/deskconn/xlink"
)

func TestBeginShellSessionCreatesNewPTY(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	transport := &deskconnd.FakeShellTransport{}

	shellID, token, ptmx, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, transport)
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	require.NoError(t, err)
	startReader()
	assert.NotEmpty(t, shellID)
	assert.NotEmpty(t, token, "a fresh session should be issued a migration token")
	require.NotNil(t, ptmx)

	assert.True(t, p.HasPTY(shellID))
	assert.True(t, p.HasSession(shellID))
}

// TestBeginShellSessionRunsExecCommand confirms the same protocol that
// serves shell also serves exec: when ctrl.Command is set, beginShellSession
// runs that command (with its args) instead of an interactive bash shell,
// and the command's real output reaches the transport.
func TestBeginShellSessionRunsExecCommand(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	transport := &deskconnd.FakeShellTransport{}

	shellID, _, _, startReader, err := p.BeginShellSession(common.ShellControlMsg{
		Op: common.ShellOpSize, Cols: 80, Rows: 24, Command: "echo", Args: []string{"hello-exec-test"},
	}, transport)
	require.NoError(t, err)
	startReader()
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	require.Eventually(t, func() bool {
		for _, chunk := range transport.Written() {
			if bytes.Contains(chunk, []byte("hello-exec-test")) {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "exec'd command's output never reached the transport")

	// echo exits immediately, so the session should clean itself up on its own.
	require.Eventually(t, func() bool {
		return !p.HasSession(shellID)
	}, 3*time.Second, 10*time.Millisecond, "PTY session should be cleaned up once the exec'd command exits")
}

// TestCleanupShellIDKillsProcessGroup guards against a real bug: closing
// ptmx alone doesn't reliably kill a child that produces no PTY output
// (nothing ever reads it) -- the expected SIGHUP-on-hangup can race with
// startOutputReader's own concurrent blocked Read on the same file and
// silently never arrive, leaving the process running forever. cleanupShellID
// must kill the process group explicitly rather than relying on that.
func TestCleanupShellIDKillsProcessGroup(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	transport := &deskconnd.FakeShellTransport{}

	shellID, _, _, startReader, err := p.BeginShellSession(common.ShellControlMsg{
		Op: common.ShellOpSize, Cols: 80, Rows: 24, Command: "sleep", Args: []string{"30"},
	}, transport)
	require.NoError(t, err)
	startReader()

	pid := p.PID(shellID)
	require.Positive(t, pid)
	require.NoError(t, syscall.Kill(pid, 0), "sanity check: process should be running before cleanup")

	p.CleanupShellID(shellID)

	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 3*time.Second, 20*time.Millisecond,
		"sleep should actually be killed (and reaped) by cleanupShellID, not just have its ptmx closed")
}

func TestBeginShellSessionMigrateValidToken(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	original := &deskconnd.FakeShellTransport{}
	shellID, token, _, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	startReader()
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	newTransport := &deskconnd.FakeShellTransport{}
	claimedID, _, ptmx, migrateStartReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: shellID, Token: token}, newTransport)

	require.NoError(t, err)
	migrateStartReader()
	assert.Equal(t, shellID, claimedID, "migration keeps the same shell ID, no rekeying needed")
	require.NotNil(t, ptmx)

	currentTransport := p.SessionTransport(shellID)
	tokenStillPending := p.HasMigrationToken(shellID)
	assert.Same(t, newTransport, currentTransport, "the session's transport should now be the new one")
	assert.False(t, tokenStillPending, "a used token must not be replayable")
}

func TestBeginShellSessionMigrateWrongToken(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	original := &deskconnd.FakeShellTransport{}
	shellID, _, _, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	startReader()
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	_, _, _, _, err = p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: shellID, Token: "not-the-real-token"},
		&deskconnd.FakeShellTransport{})
	assert.Error(t, err)

	currentTransport := p.SessionTransport(shellID)
	assert.Same(t, original, currentTransport, "a failed migration must not disturb the existing session")
}

func TestBeginShellSessionMigrateExpiredToken(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	original := &deskconnd.FakeShellTransport{}
	shellID, token, _, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	startReader()
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	p.ExpireMigrationToken(shellID)

	_, _, _, _, err = p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: shellID, Token: token}, &deskconnd.FakeShellTransport{})
	assert.Error(t, err)
}

func TestBeginShellSessionMigrateUnknownShellID(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	_, _, _, _, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: "no-such-shell", Token: "whatever"},
		&deskconnd.FakeShellTransport{})
	assert.Error(t, err)
}

// TestBeginShellSessionExecNonexistentCommand confirms a failed exec (e.g. a
// typo'd or missing command) reports a real error rather than silently
// closing the connection.
func TestBeginShellSessionExecNonexistentCommand(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	_, _, _, _, err := p.BeginShellSession(common.ShellControlMsg{
		Op: common.ShellOpSize, Cols: 80, Rows: 24, Command: "this-command-does-not-exist-xyz",
	}, &deskconnd.FakeShellTransport{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this-command-does-not-exist-xyz")
}

func TestEndShellInputCleansUpWhenStillOwner(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	transport := &deskconnd.FakeShellTransport{}
	shellID, _, _, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, transport)
	require.NoError(t, err)
	startReader()

	p.EndShellInput(shellID, transport)

	hasSession := p.HasSession(shellID)
	assert.False(t, hasSession, "a real disconnect (transport still owns the session) should tear down the PTY")
}

func TestEndShellInputNoOpAfterMigration(t *testing.T) {
	p := deskconnd.NewInteractiveShellSession()
	original := &deskconnd.FakeShellTransport{}
	shellID, token, _, startReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}, original)
	require.NoError(t, err)
	startReader()
	t.Cleanup(func() { p.CleanupShellID(shellID) })

	newTransport := &deskconnd.FakeShellTransport{}
	_, _, _, migrateStartReader, err := p.BeginShellSession(
		common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: shellID, Token: token}, newTransport)
	require.NoError(t, err)
	migrateStartReader()

	// The old connection dying after a successful migration must not kill
	// the PTY the new connection is now serving.
	p.EndShellInput(shellID, original)

	hasSession := p.HasSession(shellID)
	assert.True(t, hasSession, "migrating away must not tear down the PTY the new transport now owns")
}

// TestHandleQUICShellStreamEndToEnd drives shell over a net.Pipe through
// the real entry point (HandleQUICStream): routing frame, key exchange,
// size, one keystroke, then "exit\n" against a real spawned bash, checking
// output comes back decrypted and the stream ends cleanly on exit.
func TestHandleQUICShellStreamEndToEnd(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	d := deskconnd.NewShellDeskconn()
	go func() {
		op, err := xlink.ReadStreamOp(server)
		if err != nil {
			return
		}
		d.DispatchQUICOp(op, server)
	}()

	require.NoError(t, common.WriteMsg(client, common.RoutingFrame{Op: common.FSOpShell}))
	sendKey, receiveKey, err := deskconn.QuicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendQUICShellControl(client, sendKey,
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}))
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

// TestHandleQUICShellStreamIgnoresPing confirms a ShellOpPing control
// message (sent by the client purely to keep the connection's idle deadline
// from expiring during genuine silence) doesn't disrupt an otherwise-normal
// session -- the PTY keeps running and later data still reaches it.
func TestHandleQUICShellStreamIgnoresPing(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	d := deskconnd.NewShellDeskconn()
	go func() {
		op, err := xlink.ReadStreamOp(server)
		if err != nil {
			return
		}
		d.DispatchQUICOp(op, server)
	}()

	require.NoError(t, common.WriteMsg(client, common.RoutingFrame{Op: common.FSOpShell}))
	sendKey, receiveKey, err := deskconn.QuicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendQUICShellControl(client, sendKey,
		common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24}))
	ack, err := recvQUICShellEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.NotEmpty(t, ack.ackShellID)

	require.NoError(t, sendQUICShellControl(client, sendKey, common.ShellControlMsg{Op: common.ShellOpPing}))
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

func sendQUICShellControl(conn net.Conn, sendKey []byte, msg common.ShellControlMsg) error {
	envelope, err := common.BuildShellEnvelope(common.ShellMsgControl, common.MustJSON(msg), sendKey)
	if err != nil {
		return err
	}
	return common.WriteFrame(conn, envelope)
}

func sendQUICShellData(conn net.Conn, sendKey, data []byte) error {
	envelope, err := common.BuildShellEnvelope(common.ShellMsgData, data, sendKey)
	if err != nil {
		return err
	}
	return common.WriteFrame(conn, envelope)
}

func recvQUICShellEnvelope(conn net.Conn, receiveKey []byte) (shellAck, error) {
	frame, err := common.ReadFrame(conn)
	if err != nil {
		return shellAck{}, err
	}
	kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
	if err != nil {
		return shellAck{}, err
	}
	if kind != common.ShellMsgControl {
		return shellAck{}, nil
	}
	var msg common.ShellControlMsg
	if err := json.Unmarshal(plaintext, &msg); err != nil {
		return shellAck{}, err
	}
	return shellAck{ackShellID: msg.ShellID, ackToken: msg.Token}, nil
}
