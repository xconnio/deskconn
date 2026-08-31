package deskconn_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func setupDeskconnWithInstance(t *testing.T) *xconn.Session {
	t.Helper()
	callee, caller := setupRouterAndConnectSessions(t)
	d := deskconn.NewDeskconn(nil, nil, nil, false)
	t.Cleanup(d.Close)
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

func TestAISessionListHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(deskconn.ProcedureAISessionList).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestAISessionListHandlerNoMatchingSessionsReturnsEmpty(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	sessions, err := deskconn.CallAISessionList(caller, randomPath(t))
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestAISessionPullHandlerMissingKeyExchange(t *testing.T) {
	caller := setupDeskconnWithInstance(t)

	callResp := caller.Call(deskconn.ProcedureAISessionPull).Do()
	require.ErrorContains(t, callResp.Err, "no session keys")
}

func TestAISessionPullHandlerNoMatchingSessionsErrors(t *testing.T) {
	isolatedHome(t)
	caller := setupDeskconnWithInstance(t)

	_, err := deskconn.CallAISessionPull(caller, randomPath(t), "", "")
	require.ErrorContains(t, err, "no local sessions found")
}
