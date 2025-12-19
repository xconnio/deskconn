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
	ProcedureShell               = "io.xconn.deskconn.deskconnd.shell"

	ProcedureScreenBrightnessGetCloud = "io.xconn.deskconn.deskconnd.%s.screen.brightness.get"
	ProcedureScreenBrightnessSetCloud = "io.xconn.deskconn.deskconnd.%s.screen.brightness.set"
	ProcedureScreenLockCloud          = "io.xconn.deskconn.deskconnd.%s.screen.lock"
	ProcedureScreenIsLockedCloud      = "io.xconn.deskconn.deskconnd.%s.screen.islocked"
	ProcedureShellCloud               = "io.xconn.deskconn.deskconnd.%s.shell"

	ProcedureMPRISPlayers   = "io.xconn.deskconnd.mpris.players"
	ProcedureMPRISPlayPause = "io.xconn.deskconnd.mpris.playpause"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
)

type Deskconn struct {
	session      *xconn.Session
	shellSession *interactiveShellSession
	screen       *Screen
	mpris        *MPRIS
}

func NewDeskconn(session *xconn.Session, screen *Screen, mpris *MPRIS) *Deskconn {
	return &Deskconn{
		session:      session,
		shellSession: newInteractiveShellSession(),
		screen:       screen,
		mpris:        mpris,
	}
}

func (d *Deskconn) RegisterLocal(session *xconn.Session) error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureScreenBrightnessGet: d.brightnessGetHandler,
		ProcedureScreenBrightnessSet: d.brightnessSetHandler,
		ProcedureScreenLock:          d.lockScreenLockHandler,
		ProcedureScreenIsLocked:      d.lockScreenIsLockedHandler,
		ProcedureShell:               d.shellSession.handleShell(),
		ProcedureMPRISPlayers:        d.handleListPlayers,
		ProcedureMPRISPlayPause:      d.handlePlayPause,
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
		fmt.Sprintf(ProcedureShellCloud, machineID):               d.shellSession.handleShell(),
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

func (d *Deskconn) handleListPlayers(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	players, err := d.mpris.ListPlayers()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(players)
}

func (d *Deskconn) handlePlayPause(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	player, err := inv.ArgString(0)

	var playPauseErr error
	if err != nil {
		playPauseErr = d.mpris.PlayPause()
	} else {
		playPauseErr = d.mpris.PlayPausePlayer(player)
	}

	if playPauseErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, playPauseErr.Error())
	}

	return xconn.NewInvocationResult()
}
