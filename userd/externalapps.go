package userd

import (
	"context"
	"encoding/json"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// handleAppsRegister lists the apps a process on the local socket serves, e.g. media-app's
// Pictures, Videos and Documents, until its session leaves. Its argument is their JSON
// list (see app). The process registers and serves the apps' procedures itself.
func (d *Deskconn) handleAppsRegister(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	payload, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	var apps []*app
	if err := json.Unmarshal(payload, &apps); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	if err := d.capabilities.addExternal(inv.Caller(), apps); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	log.Printf("Registered apps from session %d", inv.Caller())
	return xconn.NewInvocationResult()
}

// handleAppUnregister drops the apps sessionID registered, if any, when it leaves.
func (d *Deskconn) handleAppUnregister(sessionID uint64) {
	if d.capabilities.removeExternal(sessionID) {
		log.Printf("Removed the apps of session %d", sessionID)
	}
}
