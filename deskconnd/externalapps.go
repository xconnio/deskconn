package deskconnd

import (
	"context"
	"encoding/json"
	"errors"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// externalApp is an app served by another process over deskconnd's local socket, e.g.
// media-app's Pictures, Videos and Documents. That process registers its apps with
// ProcedureAppsRegister, and they're listed until its session leaves.
type externalApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (d *Deskconn) handleAppsRegister(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	payload, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	var apps []externalApp
	if err := json.Unmarshal(payload, &apps); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	for _, app := range apps {
		if app.ID == "" || app.Name == "" {
			return xconn.NewInvocationError(common.ErrInvalidArgument, "an app needs an id and a name")
		}
	}

	d.appsMu.Lock()
	defer d.appsMu.Unlock()
	// ponytail: media-app is the only external app so far, so its index query is what
	// gets registered; map apps to their procedures once a second one comes along.
	if d.indexQuery == nil {
		resp := d.session.Register(common.ProcedureIndexQuery, d.handleIndexQuery).Do()
		if resp.Err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, resp.Err.Error())
		}
		d.indexQuery = &resp
	}
	d.externalApps[inv.Caller()] = apps
	log.Printf("Registered apps %v from session %d", apps, inv.Caller())
	return xconn.NewInvocationResult()
}

// handleAppUnregister drops the apps sessionID registered, if any, when it leaves.
func (d *Deskconn) handleAppUnregister(sessionID uint64) {
	d.appsMu.Lock()
	defer d.appsMu.Unlock()
	if _, ok := d.externalApps[sessionID]; !ok {
		return
	}
	delete(d.externalApps, sessionID)
	if len(d.externalApps) == 0 && d.indexQuery != nil {
		if err := d.indexQuery.Unregister(); err != nil && !errors.Is(err, context.Canceled) {
			log.WithError(err).Error("failed to unregister index query")
		}
		d.indexQuery = nil
	}
	log.Printf("Removed the apps of session %d", sessionID)
}

func (d *Deskconn) handleCapabilitiesList(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	d.appsMu.Lock()
	defer d.appsMu.Unlock()
	// The apps the frontend may show, shaped like the per-app capabilities.list entries.
	list := []any{}
	for _, apps := range d.externalApps {
		for _, app := range apps {
			list = append(list, map[string]any{"id": app.ID, "name": app.Name, "enabled": true, "requires": []string{}})
		}
	}
	return xconn.NewInvocationResult(list)
}
