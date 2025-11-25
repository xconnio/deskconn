package deskconnd

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedureBrightnessGet = "io.xconn.deskconnd.brightness.get"
	ProcedureBrightnessSet = "io.xconn.deskconnd.brightness.set"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
)

type Deskconnd struct {
	session    *xconn.Session
	brightness *Brightness
}

func NewDeskconnd(session *xconn.Session, brightness *Brightness) *Deskconnd {
	return &Deskconnd{
		brightness: brightness,
		session:    session,
	}
}

func (d *Deskconnd) Start() error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureBrightnessGet: d.brightnessGetHandler,
		ProcedureBrightnessSet: d.brightnessSetHandler,
	} {
		response := d.session.Register(uri, handler).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}
	return nil
}

func (d *Deskconnd) brightnessGetHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := d.brightness.GetBrightness()
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err)
	}

	return xconn.NewInvocationResult(brightness)
}

func (d *Deskconnd) brightnessSetHandler(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := inv.ArgInt64(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err)
	}

	if err := d.brightness.SetBrightness(int(brightness)); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult()
}
