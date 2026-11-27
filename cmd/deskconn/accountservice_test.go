package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
)

// cliOnly admits only the CLI's own identity, as the cloud does before the user signs in.
type cliOnly struct{}

func (cliOnly) Methods() []auth.Method { return []auth.Method{auth.MethodAnonymous} }

func (cliOnly) Authenticate(request auth.Request) (auth.Response, error) {
	if request.AuthID() != cliAuthID {
		return nil, errors.New("unknown identity")
	}
	return auth.NewResponse(cliAuthID, "cli", 0)
}

// startDeviceLogin serves a cloud whose account service answers the CLI's login start with
// a code, fails its first poll, answers the next with pending and the rest with finish; a
// nil finish fails every poll.
func startDeviceLogin(t *testing.T, finish map[string]any) {
	t.Helper()
	interval := minLoginPollInterval
	minLoginPollInterval = time.Millisecond
	t.Cleanup(func() { minLoginPollInterval = interval })
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	require.NoError(t, router.AddRealm(common.CloudRealm, &xconn.RealmConfig{
		Roles: []xconn.RealmRole{{Name: "cli", Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix",
			AllowCall: true}}}},
	}))
	service, err := xconn.ConnectInMemory(router, common.CloudRealm)
	require.NoError(t, err)

	start := func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		if key, _ := inv.ArgString(0); key != "pubkey" {
			return xconn.NewResultError("wamp.error.invalid_argument", "unexpected key")
		}
		if purpose, _ := inv.KwargString("purpose"); purpose != "attach" {
			return xconn.NewResultError("wamp.error.invalid_argument", "unexpected purpose")
		}
		return xconn.NewResult(map[string]any{"device_code": "secret", "user_code": "ABCD-EFGH",
			"verification_uri": "https://web.example/cli/authorize", "interval": 0})
	}
	require.NoError(t, service.Register(procedureCLILoginStart, start).Execute().Err)
	var polls atomic.Int32
	poll := func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		if code, _ := inv.ArgString(0); code != "secret" {
			return xconn.NewResultError("wamp.error.invalid_argument", "unknown device code")
		}
		switch n := polls.Add(1); {
		case finish == nil || n == 1:
			return xconn.NewResultError("wamp.error.no_such_procedure", "account service restarting")
		case n == 2:
			return xconn.NewResult(map[string]any{loginStatusKey: "pending"})
		}
		return xconn.NewResult(finish)
	}
	require.NoError(t, service.Register(procedureCLILoginPoll, poll).Execute().Err)

	server := xconn.NewServer(router, cliOnly{}, nil)
	t.Cleanup(func() { _ = server.Close() })
	listener, err := server.ListenQUIC(t.Context(), "127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("DESKCONN_CLOUD_QUIC_ADDRESS", listener.Address)
}

func TestAccountServiceLoginApproved(t *testing.T) {
	startDeviceLogin(t, map[string]any{loginStatusKey: "approved", "authid": "alice@example.com",
		"expires_at": "2026-12-26T00:00:00Z"})

	var url, code string
	authid, expiresAt, err := accountServiceLogin(t.Context(), "pubkey", "laptop", "attach",
		func(u, c string) { url, code = u, c })
	require.NoError(t, err)
	require.Equal(t, "https://web.example/cli/authorize", url)
	require.Equal(t, "ABCD-EFGH", code)
	require.Equal(t, "alice@example.com", authid)
	require.Equal(t, time.Date(2026, 12, 26, 0, 0, 0, 0, time.UTC), expiresAt)
}

func TestAccountServiceLoginDenied(t *testing.T) {
	startDeviceLogin(t, map[string]any{loginStatusKey: "denied"})

	_, _, err := accountServiceLogin(t.Context(), "pubkey", "laptop", "attach", func(string, string) {})
	require.ErrorContains(t, err, "denied")
}

func TestAccountServiceLoginGivesUpAfterFailedPolls(t *testing.T) {
	startDeviceLogin(t, nil)

	_, _, err := accountServiceLogin(t.Context(), "pubkey", "laptop", "attach", func(string, string) {})
	require.ErrorContains(t, err, "failed to check login")
}
