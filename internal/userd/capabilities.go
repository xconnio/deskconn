package userd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/relay"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

// app is one app of the web frontend. userd's own apps (see Deskconn.apps) come with
// their procedures: a procedure stays registered while any enabled app needs it. Apps
// served by other processes register themselves with ProcedureAppsRegister, are listed
// while that process is connected, and follow TopicCapabilitiesChanged to stop serving
// while disabled.
type app struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// KeyExchange is the procedure clients exchange keys with to call the app's
	// procedures, which are all end-to-end encrypted. Other processes' apps must serve
	// their own; userd's use ProcedureKeyExchange.
	KeyExchange string `json:"key_exchange,omitempty"`
	// Requires are the apps this one can't work without: disabling one of them disables
	// this app too, and this app can only be enabled while they all are.
	Requires []string `json:"requires,omitempty"`
	// DefaultOff apps stay disabled until an owner or admin enables them.
	DefaultOff bool                               `json:"default_off,omitempty"`
	Procedures map[string]xconn.InvocationHandler `json:"-"`
}

// These apps also gate streams (WebRTC and QUIC), which aren't procedures: terminalAppID
// shells, filesAppID file transfers.
const (
	terminalAppID = "terminal"
	filesAppID    = "files"
)

// apps is userd's own apps, in the order the frontend shows them.
func (d *Deskconn) apps() []*app {
	apps := []*app{
		{ID: filesAppID, Name: "Files", Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureFileBrowse:     d.handleFileBrowse,
			common.ProcedureFileRename:     d.handleFileRename,
			common.ProcedureFileDelete:     d.handleFileDelete,
			common.ProcedureFileCopy:       d.handleFileCopy,
			common.ProcedureFileSearch:     d.handleFileSearch,
			common.ProcedureFileThumbnails: d.handleFileThumbnails,
			common.ProcedureFileCat:        d.handleFileCat,
			common.ProcedureMountFS:        d.handleMountFS,
		}},
		{ID: terminalAppID, Name: "Terminal", Procedures: map[string]xconn.InvocationHandler{
			common.ProcedureShellIsBusy: d.handleShellIsBusy,
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
	}
	return append(apps, d.agentApps()...)
}

// capabilities tracks which apps the machine's owner/admin has enabled (persisted to
// capabilities.json) and keeps the enabled own apps' procedures registered on session.
type capabilities struct {
	sync.Mutex
	own []*app
	// external are the apps other processes registered, by their session.
	external map[uint64][]*app
	// states are the apps' enabled states that were set, by id. Apps without one are
	// enabled unless DefaultOff.
	states  map[string]bool
	path    string
	session *xconn.Session
	regs    map[string]xconn.RegisterResponse
}

func newCapabilities(own []*app, cfgDirectory string) *capabilities {
	c := &capabilities{
		own:      own,
		external: map[uint64][]*app{},
		states:   map[string]bool{},
		path:     filepath.Join(cfgDirectory, "capabilities.json"),
		regs:     map[string]xconn.RegisterResponse{},
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("capabilities: failed to read %s: %v", c.path, err)
		}
		return c
	}
	if err := json.Unmarshal(data, &c.states); err != nil {
		log.Printf("capabilities: failed to parse %s: %v", c.path, err)
	}
	return c
}

// apps returns every app: userd's own, then the external ones. c must be locked.
func (c *capabilities) apps() []*app {
	apps := slices.Clone(c.own)
	for _, sessionID := range slices.Sorted(maps.Keys(c.external)) {
		apps = append(apps, c.external[sessionID]...)
	}
	return apps
}

// isEnabled reports whether app id is enabled. c must be locked.
func (c *capabilities) isEnabled(id string) bool {
	if on, ok := c.states[id]; ok {
		return on
	}
	apps := c.apps()
	i := slices.IndexFunc(apps, func(a *app) bool { return a.ID == id })
	return i < 0 || !apps[i].DefaultOff
}

// enabled reports whether app id is enabled. A nil capabilities enables everything.
func (c *capabilities) enabled(id string) bool {
	if c == nil {
		return true
	}
	c.Lock()
	defer c.Unlock()
	return c.isEnabled(id)
}

// check returns an error saying app id is disabled, for clients of its streams, or nil if
// it's enabled.
func (c *capabilities) check(id string) error {
	if c.enabled(id) {
		return nil
	}
	return fmt.Errorf("%s is disabled on this machine, an owner or admin can enable it in the web app's settings", id)
}

// register starts registering the enabled own apps' procedures on session.
func (c *capabilities) register(session *xconn.Session) error {
	c.Lock()
	defer c.Unlock()
	c.session = session
	return c.sync()
}

// sync registers every procedure an enabled own app needs and unregisters the rest. c
// must be locked.
func (c *capabilities) sync() error {
	want := map[string]xconn.InvocationHandler{}
	for _, a := range c.own {
		if c.isEnabled(a.ID) {
			maps.Copy(want, a.Procedures)
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

// addExternal lists apps, registered by session sessionID.
func (c *capabilities) addExternal(sessionID uint64, apps []*app) error {
	c.Lock()
	defer c.Unlock()
	for _, a := range apps {
		if a.ID == "" || a.Name == "" {
			return errors.New("an app needs an id and a name")
		}
		if a.KeyExchange == "" {
			return fmt.Errorf("app %s has no key exchange: only end-to-end encrypted apps are supported", a.ID)
		}
		prefixes := append([]string{common.ProcedurePrefix}, relay.AppPrefixes()...)
		if !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(a.KeyExchange, prefix) }) {
			return fmt.Errorf("app %s's key exchange must be under one of %s for clients to reach it", a.ID,
				strings.Join(prefixes, ", "))
		}
		if slices.ContainsFunc(c.apps(), func(b *app) bool { return b.ID == a.ID }) {
			return fmt.Errorf("app %s is already registered", a.ID)
		}
	}
	c.external[sessionID] = apps
	return nil
}

// removeExternal drops the apps sessionID registered, and reports whether it had any.
func (c *capabilities) removeExternal(sessionID uint64) bool {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.external[sessionID]; !ok {
		return false
	}
	delete(c.external, sessionID)
	return true
}

// list returns every app and whether it's enabled. c must be locked.
func (c *capabilities) list() []any {
	apps := c.apps()
	result := make([]any, 0, len(apps))
	for _, a := range apps {
		keyExchange := a.KeyExchange
		if keyExchange == "" {
			keyExchange = common.ProcedureKeyExchange
		}
		result = append(result, map[string]any{
			"id": a.ID, "name": a.Name, "enabled": c.isEnabled(a.ID), "key_exchange": keyExchange,
			"requires": append([]string{}, a.Requires...),
		})
	}
	return result
}

func (c *capabilities) handleList(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	c.Lock()
	defer c.Unlock()
	return xconn.NewInvocationResult(c.list(), true)
}

// isManager reports whether inv's caller is one of the machine's owners or admins. xconn's
// bridge opens each remote caller's session with the caller's role, and nothing local calls
// the admin procedures, so local sessions (anonymous or trusted) aren't managers.
func isManager(inv *xconn.Invocation) bool {
	switch inv.CallerAuthRole() {
	case common.RoleOwner, common.RoleAdmin, common.CloudRoleOwner, common.CloudRoleAdmin:
		return true
	default:
		return false
	}
}

// handleSet enables or disables one app: args are the app id and the new enabled state.
// It returns the apps' list and whether anything changed (false if the app already was).
// Only the machine's owner and admins may call it (see ProcedureCapabilitiesSet).
func (c *capabilities) handleSet(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if !isManager(inv) {
		return xconn.NewInvocationError(common.ErrNotAuthorized, "only the machine's owner and admins may enable apps")
	}
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
	apps := c.apps()
	i := slices.IndexFunc(apps, func(a *app) bool { return a.ID == id })
	if i < 0 {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "app not found: "+id)
	}
	if enabled == c.isEnabled(id) {
		return xconn.NewInvocationResult(c.list(), false)
	}

	if enabled {
		for _, required := range apps[i].Requires {
			if !c.isEnabled(required) {
				return xconn.NewInvocationError(common.ErrInvalidArgument,
					fmt.Sprintf("%s requires %s, enable %s first", id, required, required))
			}
		}
		c.states[id] = true
	} else {
		// Dependents go off too, and stay off until they're enabled again themselves.
		c.states[id] = false
		for _, a := range apps {
			if slices.Contains(a.Requires, id) {
				c.states[a.ID] = false
			}
		}
	}
	if err := c.sync(); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	data, err := json.MarshalIndent(c.states, "", "  ")
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	if err := os.WriteFile(c.path, append(data, '\n'), 0600); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	// External apps follow along to stop or resume serving.
	if resp := c.session.Publish(common.TopicCapabilitiesChanged).Args(c.list()).Do(); resp.Err != nil {
		log.Printf("capabilities: failed to publish change: %v", resp.Err)
	}
	return xconn.NewInvocationResult(c.list(), true)
}
