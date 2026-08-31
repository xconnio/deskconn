package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"

	log "github.com/sirupsen/logrus"

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

// Close releases resources held open for the lifetime of the process, such as the file
// indexer's database handle. Callers that tear down a Deskconn instance before process exit
// (tests, in particular - on Windows an open file blocks removal of its containing directory)
// must call this to avoid leaking the handle.
func (d *Deskconn) Close() {
	if d.indexer != nil {
		d.indexer.Close()
	}
}

func (d *Deskconn) Register(session *xconn.Session) error {
	handlers := map[string]xconn.InvocationHandler{
		ProcedureKeyExchange:     d.handleKeyExchange,
		ProcedureShellIsBusy:     d.shellSession.handleShellIsBusy(),
		ProcedureFileBrowse:      d.handleFileBrowse,
		ProcedureFileRename:      d.handleFileRename,
		ProcedureFileDelete:      d.handleFileDelete,
		ProcedureFileCopy:        d.handleFileCopy,
		ProcedureFileEdit:        d.handleFileEdit,
		ProcedureFileCat:         d.handleFileCat,
		ProcedureFileSearch:      d.handleFileSearch,
		ProcedureGitStatus:       d.handleGitStatus,
		ProcedureGitOriginal:     d.handleGitOriginal,
		ProcedurePrinterList:     d.printer.handleListPrinters,
		ProcedurePrinterPrint:    d.printer.handlePrint(),
		ProcedureDeviceInfo:      d.handleDeviceInfo,
		ProcedureDeviceIsDesktop: d.handleDeviceIsDesktop,
		ProcedureProcessList:     d.handleProcessList,
		ProcedureProcessSignal:   d.handleProcessSignal,
		ProcedureAppList:         d.handleAppList,
		ProcedureAppIcon:         d.handleAppIcon,
		ProcedureIndexQuery:      d.handleIndexQuery,
		ProcedureAISessionList:   d.handleAISessionList,
		ProcedureAISessionPull:   d.handleAISessionPull,
		ProcedurePing: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult()
		},
	}

	// Display-related RPCs depend on a session D-Bus connection (screen,
	// wallpaper, MPRIS) or PulseAudio (audio), neither of which is available
	// on a headless server. Skip registering them there.
	if d.desktop {
		maps.Copy(handlers, map[string]xconn.InvocationHandler{
			ProcedureScreenBrightnessGet:  d.brightnessGetHandler,
			ProcedureScreenBrightnessSet:  d.brightnessSetHandler,
			ProcedureScreenLock:           d.lockScreenLockHandler,
			ProcedureScreenIsLocked:       d.lockScreenIsLockedHandler,
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
			ProcedureWallpaperGet:         d.wallpaper.HandleGet,
			ProcedureWallpaperChecksum:    d.wallpaper.HandleChecksum,
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

	var args struct {
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(plaintext, &args); err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	result, err := d.files.Browse(args.Path, args.Cursor, args.Limit)
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
