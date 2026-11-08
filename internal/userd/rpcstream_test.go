package userd_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-core/bindings/go"
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
	require.NoError(t, deskconn.UploadFiles(caller, source, uploaded, true, 3))
	downloaded := filepath.Join(t.TempDir(), "downloaded")
	require.NoError(t, deskconn.DownloadFiles(caller, uploaded, downloaded, true, 3))
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
