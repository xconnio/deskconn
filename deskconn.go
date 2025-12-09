package deskconn

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedureBrightnessGet  = "io.xconn.deskconnd.brightness.get"
	ProcedureBrightnessSet  = "io.xconn.deskconnd.brightness.set"
	ProcedureScreenLock     = "io.xconn.deskconnd.screen.lock"
	ProcedureScreenIsLocked = "io.xconn.deskconnd.screen.islocked"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
)

type Deskconn struct {
	session    *xconn.Session
	brightness *Brightness
	screen     *Screen
}

func NewDeskconn(session *xconn.Session, brightness *Brightness, screen *Screen) *Deskconn {
	return &Deskconn{
		brightness: brightness,
		session:    session,
		screen:     screen,
	}
}

func (d *Deskconn) Start() error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureBrightnessGet:  d.brightnessGetHandler,
		ProcedureBrightnessSet:  d.brightnessSetHandler,
		ProcedureScreenLock:     d.lockScreenLockHandler,
		ProcedureScreenIsLocked: d.lockScreenIsLockedHandler,
	} {
		response := d.session.Register(uri, handler).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}
	return nil
}

func (d *Deskconn) brightnessGetHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := d.brightness.GetBrightness()
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

	if err := d.brightness.SetBrightness(int(brightness)); err != nil {
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
