package userd_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
)

func newRPCFeatureSession(t *testing.T) (*xconn.Session, *userd.Deskconn) {
	t.Helper()
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm("streams", &xconn.RealmConfig{AutoDiscloseCaller: true, Meta: true}))
	host, err := xconn.ConnectInMemory(router, "streams")
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	backend := userd.NewDeskconn(false, t.TempDir())
	require.NoError(t, backend.Register(host))
	t.Cleanup(func() { backend.CloseStreams() })
	caller, err := xconn.ConnectInMemory(router, "streams")
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })
	return caller, backend
}

func TestRPCFileTransfersPreserveBinaryDataAndDirectoryLayout(t *testing.T) {
	caller, _ := newRPCFeatureSession(t)
	source := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(source, "nested"), 0755))
	payload := bytes.Repeat([]byte{0, 255, 3, 128}, 2*1024*1024+1)
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "binary.dat"), payload, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "empty"), nil, 0600))
	uploaded := filepath.Join(t.TempDir(), "uploaded")
	require.NoError(t, deskconn.UploadFiles(caller, source, uploaded, true, 3, nil))
	downloaded := filepath.Join(t.TempDir(), "downloaded")
	require.NoError(t, deskconn.DownloadFiles(caller, uploaded, downloaded, true, 3, nil))
	downloadedFile := filepath.Join(downloaded, filepath.Base(uploaded), filepath.Base(source), "nested", "binary.dat")
	body, err := os.ReadFile(downloadedFile)
	require.NoError(t, err)
	require.Equal(t, payload, body)
	info, err := os.Stat(filepath.Join(downloaded, filepath.Base(uploaded), filepath.Base(source), "empty"))
	require.NoError(t, err)
	require.Zero(t, info.Size())
}

func TestRPCStreamEndpointErrorsRemainWAMPErrors(t *testing.T) {
	caller, _ := newRPCFeatureSession(t)
	_, err := common.OpenRPCStream(context.Background(), caller, "not.registered", false)
	var application *xconn.Error
	require.ErrorAs(t, err, &application)
	require.Equal(t, common.ErrNoSuchProcedure, application.URI)
}

type rpcShell struct {
	conn                net.Conn
	sendKey, receiveKey []byte
	ack                 shellAck
}

// openRPCShell opens a shell stream from session and sends ctrl as its first message.
func openRPCShell(t *testing.T, session *xconn.Session, ctrl common.ShellControlMsg) rpcShell {
	t.Helper()
	conn, err := common.OpenRPCStream(t.Context(), session, common.ProcedureShellStream, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	sendKey, receiveKey, err := deskconn.StreamClientKeyExchange(conn)
	require.NoError(t, err)
	require.NoError(t, sendQUICShellControl(conn, sendKey, ctrl))
	ack, err := recvQUICShellEnvelope(conn, receiveKey)
	require.NoError(t, err)
	return rpcShell{conn: conn, sendKey: sendKey, receiveKey: receiveKey, ack: ack}
}

// newBridgedMachine starts a machine as the daemon runs it, returning a way to call it as
// an authenticated user and its features.
func newBridgedMachine(t *testing.T) (func(authID string) *xconn.Session, *userd.Deskconn) {
	t.Helper()
	// The machine as the daemon runs it: features on the local app layer, and a bridge from
	// the machine's router giving each remote caller its own session there, as itself.
	const machineRealm = "machine"
	dir := t.TempDir()
	appRouter, appListener, appSession, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(appRouter.Close)
	t.Cleanup(func() { _ = appListener.Close() })
	host, err := xconn.ConnectInMemory(appRouter, common.LocalRealm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	backend := userd.NewDeskconn(false, dir)
	require.NoError(t, backend.Register(host))
	t.Cleanup(func() { backend.CloseStreams() })

	device, err := xconnd.NewDeviceRouter(machineRealm)
	require.NoError(t, err)
	t.Cleanup(device.Close)
	bridgeSession, err := xconn.ConnectInMemory(device, machineRealm)
	require.NoError(t, err)
	bridge, err := xconnd.NewBridge(t.Context(), bridgeSession, &xconnd.App{
		Session: appSession,
		NewSession: func(authID, authRole string) (*xconn.Session, error) {
			return connectAs(appRouter, common.LocalRealm, authID, authRole)
		},
		Prefixes: []string{common.ProcedurePrefix, common.ProcedureAdminPrefix},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })

	sessionAs := func(authID string) *xconn.Session {
		t.Helper()
		session, err := connectAs(device, machineRealm, authID, common.RoleOwner)
		require.NoError(t, err)
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	return sessionAs, backend
}

// TestRPCShellMigrationIsBoundToCaller moves a shell between two RPC sessions, as the CLI
// does from a routed to a direct connection: the authenticated caller that opened it may,
// another caller holding the same token may not.
func TestRPCShellMigrationIsBoundToCaller(t *testing.T) {
	sessionAs, _ := newBridgedMachine(t)

	routed := sessionAs("alice@example.com")
	opened := openRPCShell(t, routed, common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24})
	require.NotEmpty(t, opened.ack.ackShellID)
	require.NotEmpty(t, opened.ack.ackToken)
	migrate := common.ShellControlMsg{Op: common.ShellOpMigrate, OldID: opened.ack.ackShellID, Token: opened.ack.ackToken}

	other := sessionAs("mallory@example.com")
	stolen := openRPCShell(t, other, migrate)
	assert.Empty(t, stolen.ack.ackShellID, "another caller must not take the shell")

	direct := sessionAs("alice@example.com")
	moved := openRPCShell(t, direct, migrate)
	require.Equal(t, opened.ack.ackShellID, moved.ack.ackShellID, "the owner's other session takes the shell")

	// The shell now answers on the new stream.
	require.NoError(t, sendQUICShellData(moved.conn, moved.sendKey, []byte("echo migrated-$((6*7))\r")))
	var output []byte
	for !bytes.Contains(output, []byte("migrated-42")) {
		msg, err := recvQUICShellEnvelope(moved.conn, moved.receiveKey)
		require.NoError(t, err, "output so far: %q", output)
		output = append(output, msg.data...)
	}
	require.NoError(t, sendQUICShellData(moved.conn, moved.sendKey, []byte("exit\r")))
}

// TestRPCAgentForwardUsesAuthenticatedCaller forwards an agent as one caller claiming to be
// another: the agent is the authenticated caller's, and only its own shells get it.
func TestRPCAgentForwardUsesAuthenticatedCaller(t *testing.T) {
	sessionAs, backend := newBridgedMachine(t)

	alice := sessionAs("alice@example.com")
	forward, err := common.OpenRPCStream(t.Context(), alice, common.ProcedureAgentForwardStream, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = forward.Close() })
	sendKey, receiveKey, err := deskconn.StreamClientKeyExchange(forward)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(forward, common.AgentFwdMsgControl, common.MustJSON(
		common.AgentForwardMsg{Op: common.AgentFwdOpStart, AuthID: "bob@example.com"}), sendKey))
	_, _, err = recvPortEnvelope(forward, receiveKey)
	require.NoError(t, err)

	sockPath, ok := backend.AgentSocketPath("alice@example.com")
	require.True(t, ok)
	_, ok = backend.AgentSocketPath("bob@example.com")
	require.False(t, ok, "the agent must not be registered for the claimed AuthID")

	agentSock := func(session *xconn.Session, claimed string) string {
		t.Helper()
		shell := openRPCShell(t, session, common.ShellControlMsg{Op: common.ShellOpSize, Cols: 80, Rows: 24,
			AuthID: claimed, Command: "sh", Args: []string{"-c", "echo \"agent=[$SSH_AUTH_SOCK]\""}})
		require.NotEmpty(t, shell.ack.ackShellID)
		var output []byte
		for {
			if match := regexp.MustCompile(`agent=\[([^\]]*)\]`).FindSubmatch(output); match != nil {
				return string(match[1])
			}
			msg, err := recvQUICShellEnvelope(shell.conn, shell.receiveKey)
			require.NoError(t, err, "output so far: %q", output)
			output = append(output, msg.data...)
		}
	}
	assert.Equal(t, sockPath, agentSock(alice, "bob@example.com"))
	assert.NotEqual(t, sockPath, agentSock(sessionAs("bob@example.com"), "alice@example.com"))
}
