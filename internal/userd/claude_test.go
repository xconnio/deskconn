package userd_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-go"
)

func setupDeskconnWithInstance(t *testing.T) *xconn.Session {
	t.Helper()
	callee, caller := setupRouterAndConnectSessions(t)
	d := userd.NewDeskconn(false, t.TempDir())
	require.NoError(t, d.Register(callee))
	return caller
}

// randomPath returns a random project path relative to $HOME - production code always treats
// the project path this way, never as an absolute path, so tests must too.
func randomPath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("ai-rpc-test-%d", time.Now().UnixNano())
}

// isolatedHome points $HOME at a fresh temp directory for the duration of the test, so the RPC
// handlers under test (which always resolve os.UserHomeDir() for real) operate against a
// throwaway home instead of the real one.
func isolatedHome(t *testing.T) string {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	return homeDir
}

func TestClaudeSessionListHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(common.ProcedureClaudeSessionList).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestClaudeSessionListHandlerNoMatchingSessionsReturnsEmpty(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	sessions, err := deskconn.CallAgentSessionList(caller, common.ClaudeTool, randomPath(t))
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestClaudeSessionPullHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(common.ProcedureClaudeSessionPull).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestClaudeSessionPullHandlerNoMatchingSessionsErrors(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	_, err := deskconn.CallAgentSessionPull(caller, common.ClaudeTool, randomPath(t), "")
	require.ErrorContains(t, err, "no local sessions found")
}
