package connectivity_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
	api "github.com/xconnio/xconn/bindings/go"
	"github.com/xconnio/xconn/bindings/go/auth"
	"github.com/xconnio/xconn/bindings/go/messages"
)

// TestExpiredKeysAreRefused follows a key's expiry through the key list and the add and remove
// events: an expired login key is refused, and keys without an expiry keep working.
func TestExpiredKeysAreRefused(t *testing.T) {
	const id, role = "alice", "member"
	past, future := time.Now().Add(-time.Minute).Unix(), time.Now().Add(time.Hour).Unix()
	login := func(policy *app.Authenticator, key string) error {
		hello := messages.NewHello(deviceRealm, id, nil, nil, []string{"cryptosign"})
		_, err := policy.Authenticate(auth.NewCryptoSignRequest(hello, key))
		return err
	}
	add := func(policy *app.Authenticator, args ...any) {
		require.NoError(t, policy.AddKey(api.NewEvent(append([]any{id}, args...), nil, nil)))
	}

	policy := app.NewAuthenticator([]*app.CryptosignPrincipal{{
		AuthID: id, AuthorizedKeys: []string{"expired", "valid", "device"}, AuthRole: role,
		KeyExpiresAt: map[string]int64{"expired": past, "valid": future},
	}}, "")
	require.ErrorContains(t, login(policy, "expired"), "expired")
	require.NoError(t, login(policy, "valid"))
	require.NoError(t, login(policy, "device"), "a key without an expiry doesn't expire")

	add(policy, "new", role, past)
	require.ErrorContains(t, login(policy, "new"), "expired")
	add(policy, "new", role, future)
	require.NoError(t, login(policy, "new"), "a key added twice expires last")
	add(policy, "new", role, past)
	require.NoError(t, login(policy, "new"), "an earlier expiry doesn't shorten a key")

	add(policy, "device", role, past)
	require.NoError(t, login(policy, "device"), "an expiry doesn't apply to a key that has none")
	add(policy, "expired", role)
	require.NoError(t, login(policy, "expired"), "a key added without an expiry no longer expires")
	add(policy, "older", role)
	require.NoError(t, login(policy, "older"), "older account services send no expiry")
	require.Error(t, policy.AddKey(api.NewEvent([]any{id, "bad", role, "soon"}, nil, nil)))
	require.Error(t, login(policy, "bad"), "a malformed expiry doesn't add the key")

	require.NoError(t, policy.RemoveKeys(api.NewEvent([]any{map[string]any{id: []any{"new"}}}, nil, nil)))
	require.Error(t, login(policy, "new"))
	add(policy, "new", role, past)
	require.ErrorContains(t, login(policy, "new"), "expired", "a removed key's expiry is forgotten")
}
