package deskconn_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-core/bindings/go"
)

func connectFileCatSessions(t *testing.T) *xconn.Session {
	t.Helper()
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))

	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = callee.Close() })
	require.NoError(t, userd.NewDeskconn(false, t.TempDir()).Register(callee))

	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = caller.Close() })
	return caller
}

func TestReadFileEncrypted(t *testing.T) {
	caller := connectFileCatSessions(t)

	for _, size := range []int{0, 11, 2*1024*1024 + 57} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.bin")
			payload := bytes.Repeat([]byte("x"), size)
			require.NoError(t, os.WriteFile(path, payload, 0600))

			data, err := deskconn.ReadFile(caller, path)
			require.NoError(t, err)
			require.True(t, bytes.Equal(payload, data))
		})
	}
}

func TestReadFileMissingKeepsPathOutOfError(t *testing.T) {
	caller := connectFileCatSessions(t)

	path := filepath.Join(t.TempDir(), "secret-name.txt")
	_, err := deskconn.ReadFile(caller, path)
	require.ErrorContains(t, err, "no such file or directory")
	require.NotContains(t, err.Error(), "secret-name")
}

func TestFileCatRejectsPlainPath(t *testing.T) {
	caller := connectFileCatSessions(t)

	callResp := caller.Call(common.ProcedureFileCat).Args("/etc/hostname").Execute()
	require.ErrorContains(t, callResp.Err, "call key exchange first")
}

func TestReadFileViaEncryptedProxy(t *testing.T) {
	machine := connectFileCatSessions(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sessions := userd.NewClientSessions()
	sessions.StoreMachineSession("machine", machine, nil, nil, ctx, cancel)
	defer sessions.DisconnectAll()
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	defer router.Close()
	require.NoError(t, router.AddRealm("local", xconn.DefaultRealmConfig()))
	backend, err := xconn.ConnectInMemory(router, "local")
	require.NoError(t, err)
	defer backend.Close()
	require.NoError(t, backend.Register(common.ProcedureProxyCat,
		userd.ProxyCatHandler(sessions, t.TempDir())).Execute().Err)
	caller, err := xconn.ConnectInMemory(router, "local")
	require.NoError(t, err)
	defer caller.Close()

	path := filepath.Join(t.TempDir(), "secret.bin")
	payload := bytes.Repeat([]byte("secret"), 400000)
	require.NoError(t, os.WriteFile(path, payload, 0600))
	data, err := deskconn.ReadFileViaProxy(caller, "machine", path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, data))
}

func TestReadFileReportsFailureAfterProgress(t *testing.T) {
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	defer router.Close()
	require.NoError(t, router.AddRealm("local", xconn.DefaultRealmConfig()))
	backend, err := xconn.ConnectInMemory(router, "local")
	require.NoError(t, err)
	defer backend.Close()
	require.NoError(t, backend.Register(common.ProcedureProxyCat,
		func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
			if err := inv.SendProgress([]any{[]byte("partial")}, nil); err != nil {
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}
			return xconn.NewResultError(common.ErrOperationFailed, "read failed")
		}).Execute().Err)
	caller, err := xconn.ConnectInMemory(router, "local")
	require.NoError(t, err)
	defer caller.Close()

	data, err := deskconn.ReadFileViaProxy(caller, "machine", "unused-file")
	require.Equal(t, []byte("partial"), data)
	require.ErrorContains(t, err, "read failed")
}
