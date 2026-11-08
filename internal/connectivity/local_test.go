package connectivity_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	app "github.com/xconnio/deskconn/internal/connectivity"
	api "github.com/xconnio/xconn-core/bindings/go"
)

func TestLoopbackAppRequiresPrivateCredentialsAndCarriesStreams(t *testing.T) {
	const realm = "local"
	const procedure = "echo"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	router, err := api.NewRouter(nil)
	require.NoError(t, err)
	defer router.Close()
	require.NoError(t, router.AddRealm(realm, &api.RealmConfig{
		Roles: []api.RealmRole{{Name: "anonymous", Permissions: []api.Permission{{
			URI: "", MatchPolicy: "prefix", AllowCall: true, AllowRegister: true,
		}}}},
	}))
	path := filepath.Join(t.TempDir(), "app.sock")
	listener, err := app.ListenLoopbackApp(router, path)
	require.NoError(t, err)
	defer listener.Close()
	anonymous, err := api.ConnectAnonymous(ctx, "yamux://"+listener.Address, realm)
	if anonymous != nil {
		defer anonymous.Close()
	}
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var endpoint map[string]string
	require.NoError(t, json.Unmarshal(data, &endpoint))
	endpoint["secret"] = "incorrect"
	wrong, err := json.Marshal(endpoint)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, wrong, 0600))
	rejected, err := app.ConnectLoopbackApp(ctx, path, realm)
	if rejected != nil {
		defer rejected.Close()
	}
	require.Error(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	session, err := app.ConnectLoopbackApp(ctx, path, realm)
	require.NoError(t, err)
	defer session.Close()
	require.NoError(t, session.Register(procedure, func(context.Context, *api.Invocation) *api.Result {
		endpoint, worker := net.Pipe()
		go func() {
			defer worker.Close()
			_, _ = io.Copy(worker, worker)
		}()
		return api.NewResult(api.DuplexStream(endpoint))
	}).ExecuteContext(ctx).Err)
	response := session.Call(procedure).ExecuteContext(ctx)
	require.NoError(t, response.Err)
	stream, err := response.ArgStream(0)
	require.NoError(t, err)
	_, err = stream.Write([]byte("hello"))
	require.NoError(t, err)
	reply := make([]byte, 5)
	_, err = io.ReadFull(stream, reply)
	require.NoError(t, err)
	require.Equal(t, "hello", string(reply))
	require.NoError(t, stream.Close())
	require.NoError(t, listener.Close())
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
