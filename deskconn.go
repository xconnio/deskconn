package deskconn

import (
	"context"
	"errors"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const (
	ProcedureScreenBrightnessGet = "io.xconn.deskconn.deskconnd.screen.brightness.get"
	ProcedureScreenBrightnessSet = "io.xconn.deskconn.deskconnd.screen.brightness.set"
	ProcedureScreenLock          = "io.xconn.deskconn.deskconnd.screen.lock"
	ProcedureScreenIsLocked      = "io.xconn.deskconn.deskconnd.screen.islocked"
	ProcedureShell               = "io.xconn.deskconn.deskconnd.shell"
	ProcedureExec                = "io.xconn.deskconn.deskconnd.exec"
	ProcedureFileBrowse          = "io.xconn.deskconn.deskconnd.file.browse"

	ProcedureMPRISPlayers   = "io.xconn.deskconn.deskconnd.mpris.players"
	ProcedureMPRISPlayPause = "io.xconn.deskconn.deskconnd.mpris.playpause"
	ProcedureMPRISPlay      = "io.xconn.deskconn.deskconnd.mpris.play"
	ProcedureMPRISPause     = "io.xconn.deskconn.deskconnd.mpris.pause"
	ProcedureMPRISNext      = "io.xconn.deskconn.deskconnd.mpris.next"
	ProcedureMPRISPrevious  = "io.xconn.deskconn.deskconnd.mpris.previous"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
)

type Deskconn struct {
	shellSession *interactiveShellSession
	files        *FileBrowser
	screen       *Screen
	mpris        *MPRIS
}

func NewDeskconn(screen *Screen, mpris *MPRIS) *Deskconn {
	return &Deskconn{
		shellSession: newInteractiveShellSession(),
		files:        NewFileBrowser(),
		screen:       screen,
		mpris:        mpris,
	}
}

func (d *Deskconn) Register(session *xconn.Session) error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureScreenBrightnessGet: d.brightnessGetHandler,
		ProcedureScreenBrightnessSet: d.brightnessSetHandler,
		ProcedureScreenLock:          d.lockScreenLockHandler,
		ProcedureScreenIsLocked:      d.lockScreenIsLockedHandler,
		ProcedureShell:               d.shellSession.handleShell(),
		ProcedureExec:                d.shellSession.handleExec(),
		ProcedureFileBrowse:          d.handleFileBrowse,
		ProcedureFileDownload:        d.handleFileDownload,
		ProcedureMPRISPlayers:        d.handleListPlayers,
		ProcedureMPRISPlayPause:      d.handlePlayPause,
		ProcedureMPRISPlay:           d.handlePlay,
		ProcedureMPRISPause:          d.handlePause,
		ProcedureMPRISNext:           d.handleNext,
		ProcedureMPRISPrevious:       d.handlePrevious,
	} {
		response := session.Register(uri, handler).Invoke(wampproto.InvokeLast).Do()
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

func (d *Deskconn) handlePlay(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	player, err := inv.ArgString(0)

	var playErr error
	if err != nil {
		playErr = d.mpris.Play()
	} else {
		playErr = d.mpris.PlayPlayer(player)
	}

	if playErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, playErr.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handlePause(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	player, err := inv.ArgString(0)

	var pauseErr error
	if err != nil {
		pauseErr = d.mpris.Pause()
	} else {
		pauseErr = d.mpris.PausePlayer(player)
	}

	if pauseErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, pauseErr.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleNext(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	player, err := inv.ArgString(0)

	var nextErr error
	if err != nil {
		nextErr = d.mpris.Next()
	} else {
		nextErr = d.mpris.NextPlayer(player)
	}

	if nextErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, nextErr.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handlePrevious(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	player, err := inv.ArgString(0)

	var previousErr error
	if err != nil {
		previousErr = d.mpris.Previous()
	} else {
		previousErr = d.mpris.PreviousPlayer(player)
	}

	if previousErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, previousErr.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleFileBrowse(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	pathArg, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.files.Browse(pathArg)
	if err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}

		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(result)
}
