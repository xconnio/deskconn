package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/info"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const (
	ProcedureKeyExchange         = "io.xconn.deskconn.deskconnd.key.exchange"
	ProcedureScreenBrightnessGet = "io.xconn.deskconn.deskconnd.screen.brightness.get"
	ProcedureScreenBrightnessSet = "io.xconn.deskconn.deskconnd.screen.brightness.set"
	ProcedureScreenLock          = "io.xconn.deskconn.deskconnd.screen.lock"
	ProcedureScreenIsLocked      = "io.xconn.deskconn.deskconnd.screen.islocked"
	ProcedureShell               = "io.xconn.deskconn.deskconnd.shell"
	ProcedureShellIsBusy         = "io.xconn.deskconn.deskconnd.shell.isbusy"
	ProcedureExec                = "io.xconn.deskconn.deskconnd.exec"
	ProcedureFileBrowse          = "io.xconn.deskconn.deskconnd.file.browse"
	ProcedurePrinterList         = "io.xconn.deskconn.deskconnd.printer.list"
	ProcedurePrinterPrint        = "io.xconn.deskconn.deskconnd.printer.print"
	ProcedureFileRename          = "io.xconn.deskconn.deskconnd.file.rename"
	ProcedureFileDelete          = "io.xconn.deskconn.deskconnd.file.delete"
	ProcedureFileCopy            = "io.xconn.deskconn.deskconnd.file.copy"
	ProcedureFileEdit            = "io.xconn.deskconn.deskconnd.file.edit"
	ProcedureFileSearch          = "io.xconn.deskconn.deskconnd.file.search"
	ProcedureLogs                = "io.xconn.deskconn.deskconnd.logs"
	ProcedurePing                = "io.xconn.deskconn.deskconnd.ping"
	ProcedureIndexQuery          = "io.xconn.deskconn.deskconnd.index.query"
	ProcedureWallpaperGet        = "io.xconn.deskconn.deskconnd.wallpaper.get"
	ProcedureWallpaperChecksum   = "io.xconn.deskconn.deskconnd.wallpaper.checksum"

	ProcedureMPRISPlayers   = "io.xconn.deskconn.deskconnd.mpris.players"
	ProcedureMPRISPlayPause = "io.xconn.deskconn.deskconnd.mpris.playpause"
	ProcedureMPRISPlay      = "io.xconn.deskconn.deskconnd.mpris.play"
	ProcedureMPRISPause     = "io.xconn.deskconn.deskconnd.mpris.pause"
	ProcedureMPRISNext      = "io.xconn.deskconn.deskconnd.mpris.next"
	ProcedureMPRISPrevious  = "io.xconn.deskconn.deskconnd.mpris.previous"

	ProcedureAudioMute       = "io.xconn.deskconn.deskconnd.audio.mute"
	ProcedureAudioUnmute     = "io.xconn.deskconn.deskconnd.audio.unmute"
	ProcedureAudioToggleMute = "io.xconn.deskconn.deskconnd.audio.togglemute"
	ProcedureAudioIsMuted    = "io.xconn.deskconn.deskconnd.audio.ismuted"

	ProcedureScreenshot           = "io.xconn.deskconn.deskconnd.screenshot"
	ProcedureScreenshotPermission = "io.xconn.deskconn.deskconnd.screenshot.permission"

	ErrInvalidArgument = "wamp.error.invalid_argument"
	ErrOperationFailed = "wamp.error.operation_failed"
	ErrNotAuthorized   = "wamp.error.not_authorized"

	MetaTopicSessionLeave = "wamp.session.on_leave"
)

type Deskconn struct {
	shellSession    *interactiveShellSession
	keys            *keyManager
	uploads         *uploadSessions
	forwardSessions *portForwardSessions
	reverseSessions *portReverseSessions
	files           *FileBrowser
	screen          *Screen
	mpris           *MPRIS
	audio           *Audio
	printer         *Printer
	logs            *logSessions
	indexer         *IndexService
	wallpaper       *Wallpaper
	processes       *info.ProcessMonitor
	appRegistry     *info.AppRegistry
}

func NewDeskconn(screen *Screen, mpris *MPRIS, audio *Audio) *Deskconn {
	d := &Deskconn{
		shellSession:    newInteractiveShellSession(),
		keys:            newKeyManager(),
		uploads:         newUploadSessions(),
		forwardSessions: newPortForwardSessions(),
		reverseSessions: newPortReverseSessions(),
		files:           NewFileBrowser(),
		screen:          screen,
		mpris:           mpris,
		audio:           audio,
		printer:         NewPrinter(),
		logs:            newLogSessions(),
		processes:       info.NewProcessMonitor(),
		appRegistry:     info.NewAppRegistry(),
	}
	if screen != nil {
		d.wallpaper = NewWallpaper(screen.SessionBus())
	}

	cfgDir, err := CfgDirectory()
	if err != nil {
		log.Printf("fileindex: failed to get config directory: %v", err)
		return d
	}

	indexer, err := NewIndexService(cfgDir)
	if err != nil {
		log.Printf("fileindex: failed to create index service: %v", err)
		return d
	}

	d.indexer = indexer
	return d
}

// StartIndexer starts the background file indexer.
func (d *Deskconn) StartIndexer(ctx context.Context) {
	if d.indexer != nil {
		d.indexer.Start(ctx)
	}
}

func (d *Deskconn) Register(session *xconn.Session) error {
	for uri, handler := range map[string]xconn.InvocationHandler{
		ProcedureKeyExchange:          d.handleKeyExchange,
		ProcedureScreenBrightnessGet:  d.brightnessGetHandler,
		ProcedureScreenBrightnessSet:  d.brightnessSetHandler,
		ProcedureScreenLock:           d.lockScreenLockHandler,
		ProcedureScreenIsLocked:       d.lockScreenIsLockedHandler,
		ProcedureShell:                d.shellSession.handleShell(),
		ProcedureShellIsBusy:          d.shellSession.handleShellIsBusy(),
		ProcedureExec:                 d.shellSession.handleExec(),
		ProcedureFileBrowse:           d.handleFileBrowse,
		ProcedureFileRename:           d.handleFileRename,
		ProcedureFileDelete:           d.handleFileDelete,
		ProcedureFileCopy:             d.handleFileCopy,
		ProcedureFileEdit:             d.handleFileEdit,
		ProcedureFileDownload:         d.handleFileDownload,
		ProcedureFileUpload:           d.handleFileUpload,
		ProcedureFileCat:              d.handleFileCat,
		ProcedureFileSearch:           d.handleFileSearch,
		ProcedurePrinterList:          d.printer.handleListPrinters,
		ProcedurePrinterPrint:         d.printer.handlePrint(),
		ProcedurePortForward:          d.handlePortForward,
		ProcedurePortReverse:          d.handlePortReverse,
		ProcedureDeviceInfo:           d.handleDeviceInfo,
		ProcedureProcessList:          d.handleProcessList,
		ProcedureProcessSignal:        d.handleProcessSignal,
		ProcedureAppList:              d.handleAppList,
		ProcedureAppIcon:              d.handleAppIcon,
		ProcedureMPRISPlayers:         d.handleListPlayers,
		ProcedureMPRISPlayPause:       d.handlePlayPause,
		ProcedureMPRISPlay:            d.handlePlay,
		ProcedureMPRISPause:           d.handlePause,
		ProcedureMPRISNext:            d.handleNext,
		ProcedureMPRISPrevious:        d.handlePrevious,
		ProcedureAudioMute:            d.handleAudioMute,
		ProcedureAudioUnmute:          d.handleAudioUnmute,
		ProcedureAudioToggleMute:      d.handleAudioToggleMute,
		ProcedureAudioIsMuted:         d.handleAudioIsMuted,
		ProcedureScreenshot:           d.handleScreenshot,
		ProcedureScreenshotPermission: d.handleScreenShotPermission,
		ProcedureLogs:                 d.handleLogs,
		ProcedureIndexQuery:           d.handleIndexQuery,
		ProcedureWallpaperGet:         d.wallpaper.HandleGet,
		ProcedureWallpaperChecksum:    d.wallpaper.HandleChecksum,
		ProcedureAISessionList:        d.handleAISessionList,
		ProcedureAISessionPull:        d.handleAISessionPull,
		ProcedurePing: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult()
		},
	} {
		response := session.Register(uri, handler).Invoke(wampproto.InvokeLast).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}

	subResp := session.Subscribe(MetaTopicSessionLeave, d.handleSessionLeave).Do()
	if subResp.Err != nil {
		return subResp.Err
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

func (d *Deskconn) handleAudioMute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.audio.Mute(); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleAudioUnmute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.audio.Unmute(); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleAudioToggleMute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	muted, err := d.audio.ToggleMute()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(muted)
}

func (d *Deskconn) handleAudioIsMuted(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	muted, err := d.audio.IsMuted()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(muted)
}

func (d *Deskconn) handleScreenshot(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	data, err := d.screen.Screenshot()
	if err != nil {
		log.WithError(err).Error("screenshot failed")
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleScreenShotPermission(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	_, err := CaptureScreenshot(d.screen.sessionBus)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleSessionLeave(event *xconn.Event) {
	sessionID, err := event.ArgUInt64(0)
	if err != nil {
		return
	}
	d.keys.delete(sessionID)
	d.uploads.delete(sessionID)
	d.forwardSessions.deleteCaller(sessionID)
	d.reverseSessions.stop(sessionID)
	d.logs.killAndDeleteByCaller(sessionID)
}

func (d *Deskconn) handleKeyExchange(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	clientPublicKey, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	serverPublicKey, serverPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	sharedSecret, err := PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	sendKey, err := DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	receiveKey, err := DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	d.keys.store(inv.Caller(), &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey})

	return xconn.NewInvocationResult(serverPublicKey)
}

func (d *Deskconn) handleIndexQuery(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if d.indexer == nil {
		return xconn.NewInvocationError(ErrOperationFailed, "index service unavailable")
	}

	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	var args struct {
		Categories []string          `json:"categories"`
		Cursor     map[string]string `json:"cursor"`
		Limit      int               `json:"limit"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.indexer.Query(args.Categories, args.Cursor, args.Limit)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleFileBrowse(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}
	plaintext, err := DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.files.Browse(string(plaintext))
	if err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	encryptedResult, err := EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}
