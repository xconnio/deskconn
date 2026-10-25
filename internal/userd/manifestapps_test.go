package userd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn-core/bindings/go"
)

// readManifest returns the Remote Control example's YAML files.
func readManifest(t *testing.T) map[string]string {
	t.Helper()
	files, err := common.ReadManifestFiles(filepath.Join("testdata", "manifests", "remote-control"))
	require.NoError(t, err)
	return files
}

// setupManager starts userd's app layer and returns its session, to register userd on, and
// a caller with a machine owner's role, as the cloud's bridge connects one.
func setupManager(t *testing.T) (*xconn.Session, *xconn.Session) {
	t.Helper()
	router, listener, callee, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	t.Cleanup(router.Close)
	t.Cleanup(func() { _ = listener.Close() })
	caller, err := connectAs(router, common.LocalRealm, "alice", common.CloudRoleOwner)
	require.NoError(t, err)
	return callee, caller
}

func registerManifest(caller *xconn.Session, files map[string]string) *xconn.Result {
	payload, _ := json.Marshal(files)
	resp := caller.Call(common.ProcedureManifestRegister).Args(payload).Execute()
	return resp
}

// decode converts a call's first result to out, through JSON.
func decode(t *testing.T, resp *xconn.Result, out any) {
	t.Helper()
	require.NoError(t, resp.Err)
	data, err := json.Marshal(resp.Args[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, out))
}

type listedApp struct {
	ID          string `json:"id"`
	Enabled     bool   `json:"enabled"`
	KeyExchange string `json:"key_exchange"`
	Manifest    bool   `json:"manifest"`
}

func findApp(t *testing.T, caller *xconn.Session, id string) *listedApp {
	t.Helper()
	var apps []listedApp
	decode(t, caller.Call(common.ProcedureCapabilitiesList).Execute(), &apps)
	for _, a := range apps {
		if a.ID == id {
			return &a
		}
	}
	return nil
}

func TestManifestApp(t *testing.T) {
	cfgDirectory := t.TempDir()
	callee, caller := setupManager(t)
	d := userd.NewDeskconn(false, cfgDirectory)
	require.NoError(t, d.Register(callee))

	resp := registerManifest(caller, readManifest(t))
	require.NoError(t, resp.Err)
	id, err := resp.ArgString(0)
	require.NoError(t, err)
	require.Equal(t, "remote-control", id)

	require.Equal(t, &listedApp{
		ID: id, Enabled: true, Manifest: true,
		KeyExchange: "io.xconn.remotecontrol.key.exchange",
	}, findApp(t, caller, id))
	require.False(t, findApp(t, caller, "files").Manifest)

	var resolved struct {
		Screens map[string]struct {
			Layout struct {
				Children []map[string]any `json:"children"`
			} `json:"layout"`
		} `json:"screens"`
	}
	decode(t, caller.Call(common.ProcedureManifestGet).Args(id).Execute(), &resolved)
	children := resolved.Screens["home"].Layout.Children
	require.Len(t, children, 2)

	// app-engine's blocks are left for clients to expand, with their fields.
	mute := children[1]
	require.Equal(t, "app-engine/blocks/toggle.yaml", mute["include"])
	require.Equal(t, "io.xconn.remotecontrol.audio.ismuted", mute["read"])
	require.NotContains(t, mute, "block")

	// Disabled apps can't be fetched.
	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args(id, false).Execute().Err)
	require.ErrorContains(t, caller.Call(common.ProcedureManifestGet).Args(id).Execute().Err, "disabled")
	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args(id, true).Execute().Err)

	// Re-registering replaces the app.
	require.NoError(t, registerManifest(caller, readManifest(t)).Err)

	// The app survives a restart.
	callee2, caller2 := setupManager(t)
	require.NoError(t, userd.NewDeskconn(false, cfgDirectory).Register(callee2))
	require.NoError(t, caller2.Call(common.ProcedureManifestGet).Args(id).Execute().Err)

	require.NoError(t, caller.Call(common.ProcedureManifestUnregister).Args(id).Execute().Err)
	require.Nil(t, findApp(t, caller, id))
	require.ErrorContains(t, caller.Call(common.ProcedureManifestGet).Args(id).Execute().Err, "not found")
	require.NoDirExists(t, filepath.Join(cfgDirectory, "manifests", id))
}

func TestManifestAppRejected(t *testing.T) {
	callee, caller := setupManager(t)
	require.NoError(t, userd.NewDeskconn(false, t.TempDir()).Register(callee))

	for name, test := range map[string]struct {
		change func(files map[string]string)
		err    string
	}{
		"admin call": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"io.xconn.remotecontrol.screen.lock", "io.xconn.deskconn.admin.capabilities.set", 1)
			},
			err: "call io.xconn.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"admin read": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"io.xconn.remotecontrol.audio.ismuted", "io.xconn.deskconn.admin.capabilities.set", 1)
			},
			err: "read io.xconn.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"admin toggle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"io.xconn.remotecontrol.audio.togglemute", "io.xconn.deskconn.admin.capabilities.set", 1)
			},
			err: "toggle io.xconn.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"unreachable toggle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"io.xconn.remotecontrol.audio.togglemute", "io.xconn.local.login", 1)
			},
			err: `toggle "io.xconn.local.login" must be under one of`,
		},
		"include blowup": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../blocks/b0.yaml\n"
				for i := range 30 {
					files[fmt.Sprintf("blocks/b%d.yaml", i)] = fmt.Sprintf(
						"a:\n  include: b%d.yaml\nb:\n  include: b%d.yaml\n", i+1, i+1)
				}
				files["blocks/b30.yaml"] = "x: 1\n"
			},
			err: "YAML mappings with its includes expanded",
		},
		"unreachable call": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"io.xconn.remotecontrol.screen.lock", "io.xconn.local.login", 1)
			},
			err: `call "io.xconn.local.login" must be under one of`,
		},
		"include outside the app": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../../other/lock.yaml\n"
			},
			err: "files must be inside the app's directory",
		},
		"missing include": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../blocks/lock.yaml\n"
			},
			err: "blocks/lock.yaml doesn't exist",
		},
		"include cycle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../blocks/lock.yaml\n"
				files["blocks/lock.yaml"] = "include: lock.yaml\n"
			},
			err: "blocks/lock.yaml includes itself",
		},
		"not yaml": {
			change: func(files map[string]string) { files["notes.txt"] = "" },
			err:    "notes.txt: only .yaml files are allowed",
		},
		"bad id": {
			change: func(files map[string]string) { files["app.yaml"] = "app:\n  id: Remote Control\n" },
			err:    "app.id must match",
		},
		"id taken": {
			change: func(files map[string]string) {
				files["app.yaml"] = "app:\n  id: files\n  name: Files\napi:\n  key_exchange: " +
					"io.xconn.deskconn.deskconnd.key.exchange\nentry_screen: home\nscreens:\n  - screens/home.yaml\n"
				files["screens/home.yaml"] = "id: home\n"
			},
			err: "app files is already registered",
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := readManifest(t)
			test.change(files)
			require.ErrorContains(t, registerManifest(caller, files).Err, test.err)
		})
	}
}

func TestCapabilitiesWatch(t *testing.T) {
	callee, caller := setupManager(t)
	require.NoError(t, userd.NewDeskconn(false, t.TempDir()).Register(callee))

	updates := make(chan []listedApp, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *xconn.Result, 1)
	go func() {
		done <- caller.Call(common.ProcedureCapabilitiesWatch).ProgressReceiver(func(r *xconn.Result) {
			data, _ := json.Marshal(r.Args[0])
			var apps []listedApp
			_ = json.Unmarshal(data, &apps)
			updates <- apps
		}).ExecuteContext(ctx)
	}()
	next := func() []listedApp {
		t.Helper()
		select {
		case apps := <-updates:
			return apps
		case <-time.After(5 * time.Second):
			t.Fatal("no update")
			return nil
		}
	}
	has := func(apps []listedApp, id string) *listedApp {
		for _, a := range apps {
			if a.ID == id {
				return &a
			}
		}
		return nil
	}

	require.Nil(t, has(next(), "remote-control"))

	require.NoError(t, registerManifest(caller, readManifest(t)).Err)
	require.True(t, has(next(), "remote-control").Enabled)

	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args("remote-control", false).Execute().Err)
	require.False(t, has(next(), "remote-control").Enabled)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch didn't end when cancelled")
	}
}

// Only a machine's owners and admins may register and unregister manifest apps.
func TestManifestAppNeedsManager(t *testing.T) {
	router, listener, callee, err := xconnd.StartAppLayer(common.LocalRealm, filepath.Join(t.TempDir(), "app.sock"))
	require.NoError(t, err)
	t.Cleanup(router.Close)
	t.Cleanup(func() { _ = listener.Close() })
	require.NoError(t, userd.NewDeskconn(false, t.TempDir()).Register(callee))

	member, err := connectAs(router, common.LocalRealm, "bob", "user:member")
	require.NoError(t, err)
	require.ErrorContains(t, registerManifest(member, readManifest(t)).Err, common.ErrNotAuthorized)
	require.ErrorContains(t, member.Call(common.ProcedureManifestUnregister).Args("remote-control").Execute().Err,
		common.ErrNotAuthorized)
}
