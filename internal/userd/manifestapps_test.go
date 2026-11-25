package userd_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/userd"
	"github.com/xconnio/xconn/bindings/go"
)

// readManifest returns the test app's YAML files, with PNG icons of its own
// for the app and for light's two states.
func readManifest(t *testing.T) map[string]string {
	t.Helper()
	files, err := common.ReadManifestFiles(filepath.Join("testdata", "manifests", "example"))
	require.NoError(t, err)
	files["app.yaml"] = strings.Replace(files["app.yaml"],
		"icon: https://example.com/icons/example.png", "icon: icons/app.png", 1)
	files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"], "        off: Turn on\n",
		"        off: Turn on\n      icon:\n        on: icons/light-on.png\n        off: icons/light-off.png\n", 1)
	for _, name := range testIcons() {
		files[name] = common.ManifestPNGPrefix + base64.StdEncoding.EncodeToString(testPNG(t))
	}
	return files
}

// testIcons are the PNG files readManifest adds.
func testIcons() []string {
	return []string{"icons/app.png", "icons/light-on.png", "icons/light-off.png"}
}

// testPNG returns a 1x1 PNG image.
func testPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, image.NewGray(image.Rect(0, 0, 1, 1))))
	return out.Bytes()
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
	Icon        string `json:"icon"`
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
	require.Equal(t, "example", id)

	require.Equal(t, &listedApp{
		ID: id, Icon: "icons/app.png", Enabled: true, Manifest: true,
		KeyExchange: "com.deskconn.example.key.exchange",
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
	light := children[1]
	require.Equal(t, "app-engine/blocks/toggle.yaml", light["include"])
	require.Equal(t, "com.deskconn.example.light.ison", light["read"])
	require.NotContains(t, light, "block")

	// Icons stay as the app names them; clients fetch its own PNG files one at a time.
	require.Equal(t, map[string]any{"on": "icons/light-on.png", "off": "icons/light-off.png"}, light["icon"])
	require.Equal(t, "https://example.com/icons/ping.png", children[0]["icon"])
	icon := caller.Call(common.ProcedureManifestFile).Args(id, "icons/light-off.png").Execute()
	require.NoError(t, icon.Err)
	require.Equal(t, testPNG(t), icon.Args[0])
	for _, name := range []string{"icons/missing.png", "screens/home.yaml", "../example/icons/app.png"} {
		require.ErrorContains(t, caller.Call(common.ProcedureManifestFile).Args(id, name).Execute().Err,
			"example has no PNG file "+name)
	}

	// Disabled apps can't be fetched, nor their files.
	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args(id, false).Execute().Err)
	require.ErrorContains(t, caller.Call(common.ProcedureManifestGet).Args(id).Execute().Err, "disabled")
	require.ErrorContains(t, caller.Call(common.ProcedureManifestFile).Args(id, "icons/app.png").Execute().Err,
		"disabled")
	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args(id, true).Execute().Err)

	// Re-registering replaces the app. A draft screen nothing includes yet can name a PNG
	// file, which is sent but not kept.
	files := readManifest(t)
	files["screens/draft.yaml"] = "id: draft\nchildren:\n  - icon: icons/draft.png\n"
	files["icons/draft.png"] = files["icons/app.png"]
	require.NoError(t, registerManifest(caller, files).Err)
	require.ErrorContains(t, caller.Call(common.ProcedureManifestFile).Args(id, "icons/draft.png").Execute().Err,
		"example has no PNG file icons/draft.png")
	require.NoFileExists(t, filepath.Join(cfgDirectory, "manifests", id, "icons", "draft.png"))

	// The app survives a restart, its PNG files stored as images.
	callee2, caller2 := setupManager(t)
	require.NoError(t, userd.NewDeskconn(false, cfgDirectory).Register(callee2))
	require.NoError(t, caller2.Call(common.ProcedureManifestGet).Args(id).Execute().Err)
	icon = caller2.Call(common.ProcedureManifestFile).Args(id, "icons/app.png").Execute()
	require.NoError(t, icon.Err)
	require.Equal(t, testPNG(t), icon.Args[0])
	stored, err := os.ReadFile(filepath.Join(cfgDirectory, "manifests", id, "icons", "app.png"))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(stored, []byte("\x89PNG")))

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
					"com.deskconn.example.ping", "com.deskconn.admin.capabilities.set", 1)
			},
			err: "call com.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"admin read": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"com.deskconn.example.light.ison", "com.deskconn.admin.capabilities.set", 1)
			},
			err: "read com.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"admin toggle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"com.deskconn.example.light.toggle", "com.deskconn.admin.capabilities.set", 1)
			},
			err: "toggle com.deskconn.admin.capabilities.set is only for the machine's owner and admins",
		},
		"unreachable toggle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = strings.Replace(files["screens/home.yaml"],
					"com.deskconn.example.light.toggle", "local.deskconn.login", 1)
			},
			err: `toggle "local.deskconn.login" must be under one of`,
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
					"com.deskconn.example.ping", "local.deskconn.login", 1)
			},
			err: `call "local.deskconn.login" must be under one of`,
		},
		"include outside the app": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../../other/ping.yaml\n"
			},
			err: "files must be inside the app's directory",
		},
		"missing include": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../blocks/ping.yaml\n"
			},
			err: "blocks/ping.yaml doesn't exist",
		},
		"include cycle": {
			change: func(files map[string]string) {
				files["screens/home.yaml"] = "id: home\nchildren:\n  - include: ../blocks/ping.yaml\n"
				files["blocks/ping.yaml"] = "include: ping.yaml\n"
			},
			err: "blocks/ping.yaml includes itself",
		},
		"not yaml": {
			change: func(files map[string]string) { files["notes.txt"] = "" },
			err:    "notes.txt: only .yaml and .png files are allowed",
		},
		"missing icon": {
			change: func(files map[string]string) { delete(files, "icons/light-off.png") },
			err:    "icon icons/light-off.png doesn't exist",
		},
		"icon outside the app": {
			change: func(files map[string]string) {
				files["app.yaml"] = strings.Replace(files["app.yaml"], "icons/app.png", "../other/app.png", 1)
			},
			err: "icon ../other/app.png doesn't exist",
		},
		"not a png": {
			change: func(files map[string]string) {
				files["icons/app.png"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("GIF89a"))
			},
			err: "icons/app.png: is not a PNG image",
		},
		"png not a data uri": {
			change: func(files map[string]string) { files["icons/app.png"] = "\x89PNG" },
			err:    "icons/app.png: must be a data:image/png;base64 data URI",
		},
		"icon too large": {
			change: func(files map[string]string) {
				files["icons/app.png"] = "data:image/png;base64," +
					base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64<<10)...))
			},
			err: "icons/app.png: can be at most 64 KiB",
		},
		"http icon": {
			change: func(files map[string]string) {
				files["app.yaml"] = strings.Replace(files["app.yaml"], "icons/app.png", "http://example.com/app.png", 1)
			},
			err: "icon http://example.com/app.png must be an https URL or a PNG file in the app",
		},
		"app icon per state": {
			change: func(files map[string]string) {
				files["app.yaml"] = strings.Replace(files["app.yaml"], "icon: icons/app.png",
					"icon: {on: icons/app.png, off: icons/app.png}", 1)
			},
			err: "app.icon must be one icon, not one per state",
		},
		"icons too large in all": {
			change: func(files map[string]string) {
				big := "data:image/png;base64," +
					base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 60<<10)...))
				var icons strings.Builder
				for i := range 5 {
					name := fmt.Sprintf("icons/big%d.png", i)
					files[name] = big
					fmt.Fprintf(&icons, "      - icon: %s\n", name)
				}
				files["screens/home.yaml"] += "  extra:\n    children:\n" + icons.String()
			},
			err: "an app's PNG files can be at most 256 KiB in all",
		},
		"yaml too large": {
			change: func(files map[string]string) { files["blocks/big.yaml"] = "x: " + strings.Repeat("a", 256<<10) },
			err:    "an app can have at most 256 KiB of YAML",
		},
		"bad id": {
			change: func(files map[string]string) { files["app.yaml"] = "app:\n  id: Example\n" },
			err:    "app.id must match",
		},
		"id taken": {
			change: func(files map[string]string) {
				files["app.yaml"] = "app:\n  id: files\n  name: Files\napi:\n  key_exchange: " +
					"com.deskconn.deskconnd.key.exchange\nentry_screen: home\nscreens:\n  - screens/home.yaml\n"
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

	require.Nil(t, has(next(), "example"))

	require.NoError(t, registerManifest(caller, readManifest(t)).Err)
	require.True(t, has(next(), "example").Enabled)

	require.NoError(t, caller.Call(common.ProcedureCapabilitiesSet).Args("example", false).Execute().Err)
	require.False(t, has(next(), "example").Enabled)

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
	require.ErrorContains(t, member.Call(common.ProcedureManifestUnregister).Args("example").Execute().Err,
		common.ErrNotAuthorized)
}
