package deskconnd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

// app is one web-frontend application and the procedures it needs (see Deskconn.apps).
// A procedure stays registered while any enabled app needs it, so disabling an app only
// removes what no other enabled app still uses.
type app struct {
	ID          string
	Name        string
	DesktopOnly bool
	Procedures  map[string]xconn.InvocationHandler
	// Requires are the apps this one can't work without: disabling one of them disables
	// this app too, and this app can only be enabled while they all are.
	Requires []string
	// DefaultOff apps stay disabled until an owner or admin enables them.
	DefaultOff bool
}

// These apps also gate streams (WebRTC and QUIC), which aren't procedures: terminalAppID
// shells, filesAppID file transfers.
const (
	terminalAppID = "terminal"
	filesAppID    = "files"
)

// apps is every app, in the order the frontend shows them.
func (d *Deskconn) apps() []*app {
	return []*app{
		{ID: filesAppID, Name: "Files", Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureFileBrowse: d.handleFileBrowse,
			common.ProcedureFileRename: d.handleFileRename,
			common.ProcedureFileDelete: d.handleFileDelete,
			common.ProcedureFileCopy:   d.handleFileCopy,
			common.ProcedureFileSearch: d.handleFileSearch,
			common.ProcedureFileCat:    d.handleFileCat,
		}},
		// The media apps are served by media-app (see handleIndexQuery). They open files
		// through Files' streams, hence requiring it.
		{ID: "pictures", Name: "Pictures", Requires: []string{filesAppID}, DefaultOff: true,
			Procedures: map[string]xconn.InvocationHandler{common.ProcedureIndexQuery: d.handleIndexQuery}},
		{ID: "videos", Name: "Videos", Requires: []string{filesAppID}, DefaultOff: true,
			Procedures: map[string]xconn.InvocationHandler{common.ProcedureIndexQuery: d.handleIndexQuery}},
		{ID: "documents", Name: "Documents", Requires: []string{filesAppID}, DefaultOff: true,
			Procedures: map[string]xconn.InvocationHandler{common.ProcedureIndexQuery: d.handleIndexQuery}},
		{ID: terminalAppID, Name: "Terminal", Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureShellIsBusy: d.shellSession.handleShellIsBusy(),
		}},
		{ID: "resource-monitor", Name: "Resource Monitor", Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureProcessList:   d.handleProcessList,
			common.ProcedureProcessSignal: d.handleProcessSignal,
			common.ProcedureAppList:       d.handleAppList,
			common.ProcedureAppIcon:       d.handleAppIcon,
		}},
		{ID: "text-editor", Name: "Text Editor", Requires: []string{filesAppID},
			Procedures: map[string]xconn.InvocationHandler{
				common.ProcedureFileBrowse:  d.handleFileBrowse,
				common.ProcedureFileCat:     d.handleFileCat,
				common.ProcedureFileEdit:    d.handleFileEdit,
				common.ProcedureGitStatus:   d.handleGitStatus,
				common.ProcedureGitOriginal: d.handleGitOriginal,
			}},
		{ID: "screenshot", Name: "Screenshot", DesktopOnly: true, Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureScreenshot:           d.handleScreenshot,
			common.ProcedureScreenshotPermission: d.handleScreenShotPermission,
		}},
	}
}

// capabilities tracks which apps the machine's owner/admin has disabled (persisted to
// capabilities.json as each app's enabled state) and keeps the enabled apps' procedures
// registered on session.
type capabilities struct {
	sync.Mutex
	apps     []*app
	disabled map[string]bool
	path     string
	session  *xconn.Session
	regs     map[string]xconn.RegisterResponse
}

func newCapabilities(apps []*app, desktop bool, cfgDirectory string) *capabilities {
	c := &capabilities{
		disabled: map[string]bool{},
		path:     filepath.Join(cfgDirectory, "capabilities.json"),
		regs:     map[string]xconn.RegisterResponse{},
	}
	for _, a := range apps {
		// Desktop-only apps depend on a display session a headless server doesn't have.
		if desktop || !a.DesktopOnly {
			c.apps = append(c.apps, a)
		}
		if a.DefaultOff {
			c.disabled[a.ID] = true
		}
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("capabilities: failed to read %s: %v", c.path, err)
		}
		return c
	}
	var enabled map[string]bool
	if err := json.Unmarshal(data, &enabled); err != nil {
		log.Printf("capabilities: failed to parse %s: %v", c.path, err)
		return c
	}
	for id, on := range enabled {
		c.disabled[id] = !on
	}
	return c
}

// enabled reports whether app id is enabled. A nil capabilities enables everything.
func (c *capabilities) enabled(id string) bool {
	if c == nil {
		return true
	}
	c.Lock()
	defer c.Unlock()
	return !c.disabled[id]
}

// check returns an error saying app id is disabled, for clients of its streams, or nil if
// it's enabled.
func (c *capabilities) check(id string) error {
	if c.enabled(id) {
		return nil
	}
	return fmt.Errorf("%s is disabled on this device, an owner or admin can enable it with `desk apps enable %s`",
		id, id)
}

// register starts registering the enabled apps' procedures on session.
func (c *capabilities) register(session *xconn.Session) error {
	c.Lock()
	defer c.Unlock()
	c.session = session
	return c.sync()
}

// sync registers every procedure an enabled app needs and unregisters the rest. c must be locked.
func (c *capabilities) sync() error {
	want := map[string]xconn.InvocationHandler{}
	for _, a := range c.apps {
		if !c.disabled[a.ID] {
			for uri, handler := range a.Procedures {
				want[uri] = handler
			}
		}
	}

	for uri, reg := range c.regs {
		if _, ok := want[uri]; ok {
			continue
		}
		if err := reg.Unregister(); err != nil {
			return err
		}
		delete(c.regs, uri)
		log.Printf("Unregistered procedure %s", uri)
	}

	for uri, handler := range want {
		if _, ok := c.regs[uri]; ok {
			continue
		}
		resp := c.session.Register(uri, handler).Invoke(wampproto.InvokeLast).Do()
		if resp.Err != nil {
			return resp.Err
		}
		c.regs[uri] = resp
		log.Printf("Registered procedure %s", uri)
	}
	return nil
}

// list returns every app and whether it's enabled. c must be locked.
func (c *capabilities) list() []any {
	result := make([]any, 0, len(c.apps))
	for _, a := range c.apps {
		result = append(result, map[string]any{
			"id": a.ID, "name": a.Name, "enabled": !c.disabled[a.ID], "requires": append([]string{}, a.Requires...),
		})
	}
	return result
}

func (c *capabilities) handleList(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	c.Lock()
	defer c.Unlock()
	return xconn.NewInvocationResult(c.list(), true)
}

// handleSet enables or disables one app: args are the app id and the new enabled state.
// It returns the apps' list and whether anything changed (false if the app already was).
// Only the device's owner and admins should reach it: deskconn-router checks cloud calls
// (user:admin). Direct LAN/P2P connections, served by xconn-go's device router, aren't
// checked yet.
func (c *capabilities) handleSet(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	id, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	enabled, err := inv.ArgBool(1)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	c.Lock()
	defer c.Unlock()
	i := slices.IndexFunc(c.apps, func(a *app) bool { return a.ID == id })
	if i < 0 {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "app not found: "+id)
	}
	if enabled == !c.disabled[id] {
		return xconn.NewInvocationResult(c.list(), false)
	}

	if enabled {
		for _, required := range c.apps[i].Requires {
			if c.disabled[required] {
				return xconn.NewInvocationError(common.ErrInvalidArgument,
					fmt.Sprintf("%s requires %s, enable %s first", id, required, required))
			}
		}
		delete(c.disabled, id)
	} else {
		// Dependents go off too, and stay off until they're enabled again themselves.
		c.disabled[id] = true
		for _, a := range c.apps {
			if slices.Contains(a.Requires, id) {
				c.disabled[a.ID] = true
			}
		}
	}
	if err := c.sync(); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	// Every app's state, so a default-off app that was enabled stays enabled.
	enabledByID := make(map[string]bool, len(c.apps))
	for _, a := range c.apps {
		enabledByID[a.ID] = !c.disabled[a.ID]
	}
	data, err := json.MarshalIndent(enabledByID, "", "  ")
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	if err := os.WriteFile(c.path, append(data, '\n'), 0600); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	// Lets apps outside deskconnd (media-app) follow along.
	if resp := c.session.Publish(common.TopicCapabilitiesChanged).Args(c.list()).Do(); resp.Err != nil {
		log.Printf("capabilities: failed to publish change: %v", resp.Err)
	}
	return xconn.NewInvocationResult(c.list(), true)
}
