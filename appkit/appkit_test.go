package appkit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/appkit"
	"github.com/xconnio/xconn-go"
)

const (
	keyExchange = "io.xconn.test.key.exchange"
	echo        = "io.xconn.test.echo"
)

// An app serves an encrypted procedure, and a client calls it with its own keys.
func TestEncryptedProcedure(t *testing.T) {
	router, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, router.AddRealm(appkit.LocalRealm, &xconn.RealmConfig{AutoDiscloseCaller: true}))
	t.Cleanup(router.Close)

	server, err := xconn.ConnectInMemory(router, appkit.LocalRealm)
	require.NoError(t, err)
	enc := appkit.NewEncryption()
	require.NoError(t, enc.Register(server, keyExchange))
	require.NoError(t, server.Register(echo, enc.Encrypted(func(_ context.Context, req []byte) (any, error) {
		return string(req), nil
	})).Do().Err)

	client, err := xconn.ConnectInMemory(router, appkit.LocalRealm)
	require.NoError(t, err)
	keys, err := appkit.ClientKeyExchange(client, keyExchange)
	require.NoError(t, err)
	out, err := appkit.EncryptedCall(client, echo, []byte("hello"), keys)
	require.NoError(t, err)
	require.JSONEq(t, `"hello"`, string(out))

	// Without the keys the call fails.
	require.Error(t, client.Call(echo).Args([]byte("hello")).Do().Err)
}

func TestSocketURI(t *testing.T) {
	require.Contains(t, appkit.SocketURI("/home/alice/.deskconn"), "/home/alice/.deskconn/deskconn.sock")
}
