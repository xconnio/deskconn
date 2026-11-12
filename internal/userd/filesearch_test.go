package userd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	connectivity "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-core/bindings/go"
)

func TestEncryptedFileSearchStreamsThroughTheRustBridge(t *testing.T) {
	const (
		searchQuery = "match"
		pathField   = "path"
	)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	local, listener, backend, err := connectivity.StartAppLayer("local", filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	defer local.Close()
	defer listener.Close()
	defer backend.Close()
	require.NoError(t, userd.NewDeskconn(false, t.TempDir()).Register(backend))
	remote, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	defer remote.Close()
	require.NoError(t, remote.AddRealm("remote", &xconn.RealmConfig{AutoDiscloseCaller: true, Meta: true}))
	gateway, err := xconn.ConnectInMemory(remote, "remote")
	require.NoError(t, err)
	defer gateway.Close()
	bridge, err := connectivity.NewBridge(ctx, gateway, &connectivity.App{
		Session:    backend,
		Prefixes:   []string{common.ProcedurePrefix},
		NewSession: func(id, role string) (*xconn.Session, error) { return connectAs(local, "local", id, role) },
	})
	require.NoError(t, err)
	defer bridge.Close()
	caller, err := connectAs(remote, "remote", "alice", "trusted")
	require.NoError(t, err)
	defer caller.Close()
	keys, err := common.ClientKeyExchange(caller)
	require.NoError(t, err)
	directory := tempDirInHome(t)
	for index := range 105 {
		require.NoError(
			t,
			os.WriteFile(filepath.Join(directory, fmt.Sprintf("match-%03d.txt", index)), []byte("test"), 0600),
		)
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".match-hidden"), nil, 0600))
	require.NoError(t, os.Mkdir(filepath.Join(directory, ".hidden"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".hidden", "match.txt"), nil, 0600))

	for _, testCase := range []struct {
		name       string
		query      string
		showHidden bool
		matches    int
	}{
		{name: "visible", query: searchQuery, matches: 105},
		{name: "include-hidden", query: searchQuery, showHidden: true, matches: 107},
		{name: "empty-result", query: "nothing-matches"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plaintext, err := json.Marshal(
				map[string]any{pathField: directory, "query": testCase.query, "show_hidden": testCase.showHidden},
			)
			require.NoError(t, err)
			encrypted, err := common.EncryptPayload(plaintext, keys.SendKey)
			require.NoError(t, err)
			call, err := caller.Call(common.ProcedureFileSearch).Arg(encrypted).StreamContext(ctx)
			require.NoError(t, err)
			defer call.Close()
			var entries []common.FileEntry
			progress, final := 0, 0
			for {
				result, err := call.Receive(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if !result.Progress() {
					final++
					continue
				}
				progress++
				payload, err := result.ArgBytes(0)
				require.NoError(t, err)
				decoded, err := common.DecryptPayload(payload, keys.ReceiveKey)
				require.NoError(t, err)
				var batch []common.FileEntry
				require.NoError(t, json.Unmarshal(decoded, &batch))
				entries = append(entries, batch...)
			}
			require.Equal(t, 1, final)
			if testCase.matches > 0 {
				require.Equal(t, 3, progress)
				require.Len(t, entries, testCase.matches)
				if !testCase.showHidden {
					for _, entry := range entries {
						require.NotContains(t, filepath.Base(entry.Path), "hidden")
					}
				}
			} else {
				require.Zero(t, progress)
				require.Empty(t, entries)
			}
		})
	}
	invalidRequest, err := json.Marshal(map[string]any{
		pathField: filepath.Join(directory, "missing"), "query": searchQuery,
	})
	require.NoError(t, err)
	encrypted, err := common.EncryptPayload(invalidRequest, keys.SendKey)
	require.NoError(t, err)
	failed, err := caller.Call(common.ProcedureFileSearch).Arg(encrypted).StreamContext(ctx)
	require.NoError(t, err)
	defer failed.Close()
	_, err = failed.Receive(ctx)
	require.ErrorContains(t, err, common.ErrInvalidArgument)
	require.NoError(t, caller.Call(common.ProcedurePing).ExecuteContext(ctx).Err)
}
