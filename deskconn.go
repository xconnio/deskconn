package deskconn

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedureScreenBrightnessGet = "io.xconn.deskconn.deskconnd.screen.brightness.get"
	ProcedureScreenBrightnessSet = "io.xconn.deskconn.deskconnd.screen.brightness.set"
	ProcedureScreenLock          = "io.xconn.deskconn.deskconnd.screen.lock"
	ProcedureScreenIsLocked      = "io.xconn.deskconn.deskconnd.screen.islocked"

	ProcedureScreenBrightnessGetCloud = "io.xconn.deskconn.deskconnd.%s.screen.brightness.get"
	ProcedureScreenBrightnessSetCloud = "io.xconn.deskconn.deskconnd.%s.screen.brightness.set"
	ProcedureScreenLockCloud          = "io.xconn.deskconn.deskconnd.%s.screen.lock"
	ProcedureScreenIsLockedCloud      = "io.xconn.deskconn.deskconnd.%s.screen.islocked"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
)

type Deskconn struct {
	screen *Screen
}

func NewDeskconn(screen *Screen) *Deskconn {
	return &Deskconn{
		screen: screen,
	}
}

func (d *Deskconn) RegisterLocal(session *xconn.Session) error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureScreenBrightnessGet: d.brightnessGetHandler,
		ProcedureScreenBrightnessSet: d.brightnessSetHandler,
		ProcedureScreenLock:          d.lockScreenLockHandler,
		ProcedureScreenIsLocked:      d.lockScreenIsLockedHandler,
	} {
		response := session.Register(uri, handler).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}
	return nil
}

func (d *Deskconn) RegisterCloud(session *xconn.Session, machineID string) error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		fmt.Sprintf(ProcedureScreenBrightnessGetCloud, machineID): d.brightnessGetHandler,
		fmt.Sprintf(ProcedureScreenBrightnessSetCloud, machineID): d.brightnessSetHandler,
		fmt.Sprintf(ProcedureScreenLockCloud, machineID):          d.lockScreenLockHandler,
		fmt.Sprintf(ProcedureScreenIsLockedCloud, machineID):      d.lockScreenIsLockedHandler,
	} {
		response := session.Register(uri, handler).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}
	return nil
}

func (d *Deskconn) brightnessGetHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := d.screen.GetBrightness()
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	return xconn.NewInvocationResult(brightness)
}

func (d *Deskconn) brightnessSetHandler(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := inv.ArgInt64(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err)
	}

	if err := d.screen.SetBrightness(int(brightness)); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) lockScreenLockHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.screen.Lock(); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) lockScreenIsLockedHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	isLocked, err := d.screen.IsLocked()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult(isLocked)
}
