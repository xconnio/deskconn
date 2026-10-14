package deskconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/xconn-go"
)

func connectFileCatSessions(t *testing.T) *xconn.Session {
	t.Helper()
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))

	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	require.NoError(t, deskconnd.NewDeskconn(nil, false, t.TempDir()).Register(callee))

	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	return caller
}

func TestReadFileEncrypted(t *testing.T) {
	caller := connectFileCatSessions(t)

	path := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0600))

	data, err := deskconn.ReadFile(caller, path)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(data))
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

	callResp := caller.Call(common.ProcedureFileCat).Args("/etc/hostname").Do()
	require.ErrorContains(t, callResp.Err, "call key exchange first")
}
