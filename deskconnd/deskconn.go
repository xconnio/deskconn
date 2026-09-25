package deskconnd

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/info"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const MetaTopicSessionLeave = "wamp.session.on_leave"

type Deskconn struct {
	shellSession         *interactiveShellSession
	keys                 *keyManager
	agentForwardSessions *agentForwardSessions
	files                *FileBrowser
	screen               *Screen
	mpris                *MPRIS
	audio                *Audio
	printer              *Printer
	indexer              *IndexService
	wallpaper            *Wallpaper
	processes            *info.ProcessMonitor
	appRegistry          *info.AppRegistry
	desktop              bool
	vpn                  *vpnServer
}

func NewDeskconn(screen *Screen, mpris *MPRIS, audio *Audio, desktopEnvironment bool, cfgDirectory string) *Deskconn {
	d := &Deskconn{
		shellSession:         newInteractiveShellSession(),
		keys:                 newKeyManager(),
		agentForwardSessions: newAgentForwardSessions(),
		files:                NewFileBrowser(),
		screen:               screen,
		mpris:                mpris,
		audio:                audio,
		printer:              NewPrinter(),
		processes:            info.NewProcessMonitor(),
		appRegistry:          info.NewAppRegistry(),
		desktop:              desktopEnvironment,
		vpn:                  newVPNServer(),
	}
	d.shellSession.agentForward = d.agentForwardSessions
	if screen != nil {
		d.wallpaper = NewWallpaper(screen.SessionBus())
	}

	indexer, err := NewIndexService(cfgDirectory)
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
	handlers := map[string]xconn.InvocationHandler{
		common.ProcedureKeyExchange:     d.handleKeyExchange,
		common.ProcedureShellIsBusy:     d.shellSession.handleShellIsBusy(),
		common.ProcedureFileBrowse:      d.handleFileBrowse,
		common.ProcedureFileRename:      d.handleFileRename,
		common.ProcedureFileDelete:      d.handleFileDelete,
		common.ProcedureFileCopy:        d.handleFileCopy,
		common.ProcedureFileEdit:        d.handleFileEdit,
		common.ProcedureFileCat:         d.handleFileCat,
		common.ProcedureFileSearch:      d.handleFileSearch,
		common.ProcedureGitStatus:       d.handleGitStatus,
		common.ProcedureGitOriginal:     d.handleGitOriginal,
		common.ProcedurePrinterList:     d.printer.handleListPrinters,
		common.ProcedurePrinterPrint:    d.printer.handlePrint(),
		common.ProcedureDeviceInfo:      d.handleDeviceInfo,
		common.ProcedureDeviceIsDesktop: d.handleDeviceIsDesktop,
		common.ProcedureProcessList:     d.handleProcessList,
		common.ProcedureProcessSignal:   d.handleProcessSignal,
		common.ProcedureAppList:         d.handleAppList,
		common.ProcedureAppIcon:         d.handleAppIcon,
		common.ProcedureIndexQuery:      d.handleIndexQuery,
		common.ProcedureAISessionList:   d.handleAISessionList,
		common.ProcedureAISessionPull:   d.handleAISessionPull,
		common.ProcedurePing: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult()
		},
	}

	// Display-related RPCs depend on a session D-Bus connection (screen,
	// wallpaper, MPRIS) or PulseAudio (audio), neither of which is available
	// on a headless server. Skip registering them there.
	if d.desktop {
		maps.Copy(handlers, map[string]xconn.InvocationHandler{
			common.ProcedureScreenBrightnessGet:  d.brightnessGetHandler,
			common.ProcedureScreenBrightnessSet:  d.brightnessSetHandler,
			common.ProcedureScreenLock:           d.lockScreenLockHandler,
			common.ProcedureScreenIsLocked:       d.lockScreenIsLockedHandler,
			common.ProcedureMPRISPlayers:         d.handleListPlayers,
			common.ProcedureMPRISPlayPause:       d.handlePlayPause,
			common.ProcedureMPRISPlay:            d.handlePlay,
			common.ProcedureMPRISPause:           d.handlePause,
			common.ProcedureMPRISNext:            d.handleNext,
			common.ProcedureMPRISPrevious:        d.handlePrevious,
			common.ProcedureAudioMute:            d.handleAudioMute,
			common.ProcedureAudioUnmute:          d.handleAudioUnmute,
			common.ProcedureAudioToggleMute:      d.handleAudioToggleMute,
			common.ProcedureAudioIsMuted:         d.handleAudioIsMuted,
			common.ProcedureScreenshot:           d.handleScreenshot,
			common.ProcedureScreenshotPermission: d.handleScreenShotPermission,
			common.ProcedureWallpaperGet:         d.wallpaper.HandleGet,
			common.ProcedureWallpaperChecksum:    d.wallpaper.HandleChecksum,
		})
	}

	for uri, handler := range handlers {
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
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	return xconn.NewInvocationResult(brightness)
}

func (d *Deskconn) brightnessSetHandler(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	brightness, err := inv.ArgInt64(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err)
	}

	if err := d.screen.SetBrightness(int(brightness)); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) lockScreenLockHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.screen.Lock(); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) lockScreenIsLockedHandler(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	isLocked, err := d.screen.IsLocked()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err)
	}

	return xconn.NewInvocationResult(isLocked)
}

func (d *Deskconn) handleListPlayers(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	players, err := d.mpris.ListPlayers()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
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
		return xconn.NewInvocationError(common.ErrOperationFailed, playPauseErr.Error())
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
		return xconn.NewInvocationError(common.ErrOperationFailed, playErr.Error())
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
		return xconn.NewInvocationError(common.ErrOperationFailed, pauseErr.Error())
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
		return xconn.NewInvocationError(common.ErrOperationFailed, nextErr.Error())
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
		return xconn.NewInvocationError(common.ErrOperationFailed, previousErr.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleAudioMute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.audio.Mute(); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleAudioUnmute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	if err := d.audio.Unmute(); err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleAudioToggleMute(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	muted, err := d.audio.ToggleMute()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(muted)
}

func (d *Deskconn) handleAudioIsMuted(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	muted, err := d.audio.IsMuted()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(muted)
}

func (d *Deskconn) handleScreenshot(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	data, err := d.screen.Screenshot()
	if err != nil {
		log.WithError(err).Error("screenshot failed")
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(data)
}

func (d *Deskconn) handleScreenShotPermission(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
	_, err := CaptureScreenshot(d.screen.sessionBus)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult()
}

func (d *Deskconn) handleSessionLeave(event *xconn.Event) {
	sessionID, err := event.ArgUInt64(0)
	if err != nil {
		return
	}
	d.keys.delete(sessionID)
}

func (d *Deskconn) handleKeyExchange(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	clientPublicKey, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	serverPublicKey, serverPrivateKey, err := common.CreateX25519KeyPair()
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	sharedSecret, err := common.PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	sendKey, err := common.DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	receiveKey, err := common.DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	d.keys.store(inv.Caller(), &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey})

	return xconn.NewInvocationResult(serverPublicKey)
}

func (d *Deskconn) handleIndexQuery(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	if d.indexer == nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, "index service unavailable")
	}

	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	var args struct {
		Categories []string          `json:"categories"`
		Cursor     map[string]string `json:"cursor"`
		Limit      int               `json:"limit"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	result, err := d.indexer.Query(args.Categories, args.Cursor, args.Limit)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}

func (d *Deskconn) handleFileBrowse(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	enc, ok := d.keys.fetch(inv.Caller())
	if !ok {
		return xconn.NewInvocationError(common.ErrInvalidArgument, "no session keys found, call key exchange first")
	}

	encrypted, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	plaintext, err := common.DecryptPayload(encrypted, enc.receiveKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	var args struct {
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	result, err := d.files.Browse(args.Path, args.Cursor, args.Limit)
	if err != nil {
		if errors.Is(err, errFilePathEscapesHome) || errors.Is(err, os.ErrNotExist) {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	encryptedResult, err := common.EncryptPayload(resultBytes, enc.sendKey)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	return xconn.NewInvocationResult(encryptedResult)
}
