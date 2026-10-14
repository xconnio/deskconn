package common_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func TestAppEncryption(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	cfg := xconn.DefaultRealmConfig()
	cfg.AutoDiscloseCaller = true
	require.NoError(t, r.AddRealm("realm1", cfg))
	app, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	client, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	const keyExchange, echo, fail = "test.key.exchange", "test.echo", "test.fail"
	enc := common.NewAppEncryption()
	require.NoError(t, enc.Register(app, keyExchange))
	require.NoError(t, app.Register(echo, enc.Encrypted(func(_ context.Context, req []byte) (any, error) {
		return string(req), nil
	})).Do().Err)
	require.NoError(t, app.Register(fail, enc.Encrypted(func(_ context.Context, _ []byte) (any, error) {
		return nil, &xconn.Error{URI: common.ErrInvalidArgument, Args: []any{"bad"}}
	})).Do().Err)

	require.ErrorContains(t, client.Call(echo).Do().Err, "call key exchange first")

	keys, err := common.ClientKeyExchangeWith(client, keyExchange)
	require.NoError(t, err)
	result, err := common.EncryptedCall(client, echo, []byte("hello"), keys)
	require.NoError(t, err)
	require.JSONEq(t, `"hello"`, string(result))

	_, err = common.EncryptedCall(client, fail, nil, keys)
	require.ErrorContains(t, err, common.ErrInvalidArgument)
}
