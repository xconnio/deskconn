package deskconnd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconnd"
	"github.com/xconnio/xconn-go"
)

const filesApp = "files"

func TestCapabilitiesSharedProceduresAndPersistence(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))
	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	ok := func(context.Context, *xconn.Invocation) *xconn.InvocationResult { return xconn.NewInvocationResult() }
	apps := []*deskconnd.App{
		{ID: "a", Procedures: map[string]xconn.InvocationHandler{"shared": ok, "only.a": ok}},
		{ID: "b", Procedures: map[string]xconn.InvocationHandler{"shared": ok}},
		{ID: "desk", DesktopOnly: true},
	}
	cfgDir := t.TempDir()
	_, list, set, err := deskconnd.NewCapabilities(apps, false, cfgDir, callee)
	require.NoError(t, err)
	require.NoError(t, callee.Register("set", set).Do().Err)
	require.NoError(t, callee.Register("list", list).Do().Err)
	listResp := caller.Call("list").Do()
	require.NoError(t, listResp.Err)
	require.Len(t, listResp.Args()[0], 2, "desktop-only app hidden on headless")

	registered := func(uri string) bool { return caller.Call(uri).Do().Err == nil }
	require.True(t, registered("shared"))
	require.True(t, registered("only.a"))

	// Disabling a keeps the procedure b still needs.
	require.NoError(t, caller.Call("set").Args("a", false).Do().Err)
	require.True(t, registered("shared"))
	require.False(t, registered("only.a"))

	require.NoError(t, caller.Call("set").Args("b", false).Do().Err)
	require.False(t, registered("shared"))
	require.Error(t, caller.Call("set").Args("nope", false).Do().Err)

	// Disabled apps survive a restart; re-enabling registers again.
	enabled2, _, _, err := deskconnd.NewCapabilities(apps, false, cfgDir, callee)
	require.NoError(t, err)
	require.False(t, enabled2("a"))
	require.False(t, enabled2("b"))
	require.NoError(t, caller.Call("set").Args("a", true).Do().Err)
	require.True(t, registered("only.a"))
}

func TestCapabilitiesRequires(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))
	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	apps := []*deskconnd.App{{ID: filesApp}, {ID: "editor", Requires: []string{filesApp}}, {ID: "other"}}
	enabled, _, set, err := deskconnd.NewCapabilities(apps, true, t.TempDir(), callee)
	require.NoError(t, err)
	require.NoError(t, callee.Register("set", set).Do().Err)

	// Turning files off turns its dependents off, not unrelated apps.
	resp := caller.Call("set").Args(filesApp, false).Do()
	require.NoError(t, resp.Err)
	require.True(t, resp.ArgBoolOr(1, false), "changed")
	resp = caller.Call("set").Args(filesApp, false).Do()
	require.NoError(t, resp.Err)
	require.False(t, resp.ArgBoolOr(1, true), "already disabled")
	require.False(t, enabled("editor"))
	require.True(t, enabled("other"))

	// A dependent can't come back before what it requires, and doesn't come back on its own.
	require.ErrorContains(t, caller.Call("set").Args("editor", true).Do().Err, "enable files first")
	require.NoError(t, caller.Call("set").Args(filesApp, true).Do().Err)
	require.False(t, enabled("editor"))
	require.NoError(t, caller.Call("set").Args("editor", true).Do().Err)
	require.True(t, enabled("editor"))
}

func TestCapabilitiesDefaultOff(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))
	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	caller, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)

	apps := []*deskconnd.App{{ID: filesApp}, {ID: "media", DefaultOff: true}}
	cfgDir := t.TempDir()
	enabled, _, set, err := deskconnd.NewCapabilities(apps, true, cfgDir, callee)
	require.NoError(t, err)
	require.True(t, enabled(filesApp))
	require.False(t, enabled("media"), "off until enabled")

	// Changes are published for apps outside deskconnd (media-app).
	published := make(chan []any, 1)
	require.NoError(t, caller.Subscribe(common.TopicCapabilitiesChanged, func(e *xconn.Event) {
		published <- e.Args()
	}).Do().Err)
	require.NoError(t, callee.Register("set", set).Do().Err)
	require.NoError(t, caller.Call("set").Args("media", true).Do().Err)
	select {
	case args := <-published:
		require.Len(t, args, 1)
	case <-time.After(time.Second):
		t.Fatal("no change published")
	}

	// Once enabled, a default-off app stays enabled after a restart.
	enabled2, _, _, err := deskconnd.NewCapabilities(apps, true, cfgDir, callee)
	require.NoError(t, err)
	require.True(t, enabled2("media"))
}
