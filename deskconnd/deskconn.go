package deskconnd

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const MetaTopicSessionLeave = "wamp.session.on_leave"

type Deskconn struct {
	shellSession         *interactiveShellSession
	keys                 *keyManager
	agentForwardSessions *agentForwardSessions
	files                *FileBrowser
	wallpaper            *Wallpaper
	processes            *ProcessMonitor
	appRegistry          *AppRegistry
	desktop              bool
	vpn                  *vpnServer
	// session is the session Register registered on, also used to reach media-app's
	// procedures.
	session *xconn.Session

	appsMu sync.Mutex
	// externalApps are the apps other processes registered, by their session.
	externalApps map[uint64][]externalApp
	// indexQuery is registered while media-app's apps are.
	indexQuery *xconn.RegisterResponse
}

// NewDeskconn returns the machine's APIs. sessionBus, nil on a headless server, serves the
// wallpaper.
func NewDeskconn(sessionBus *dbus.Conn, desktopEnvironment bool) *Deskconn {
	d := &Deskconn{
		shellSession:         newInteractiveShellSession(),
		keys:                 newKeyManager(),
		agentForwardSessions: newAgentForwardSessions(),
		files:                NewFileBrowser(),
		processes:            NewProcessMonitor(),
		appRegistry:          NewAppRegistry(),
		desktop:              desktopEnvironment,
		vpn:                  newVPNServer(),
		externalApps:         map[uint64][]externalApp{},
	}
	d.shellSession.agentForward = d.agentForwardSessions
	if sessionBus != nil {
		d.wallpaper = NewWallpaper(sessionBus)
	}
	return d
}

func (d *Deskconn) Register(session *xconn.Session) error {
	d.session = session
	handlers := map[string]xconn.InvocationHandler{
		common.ProcedureKeyExchange:      d.handleKeyExchange,
		common.ProcedureShellIsBusy:      d.handleShellIsBusy,
		common.ProcedureFileBrowse:       d.handleFileBrowse,
		common.ProcedureFileRename:       d.handleFileRename,
		common.ProcedureFileDelete:       d.handleFileDelete,
		common.ProcedureFileCopy:         d.handleFileCopy,
		common.ProcedureFileEdit:         d.handleFileEdit,
		common.ProcedureFileCat:          d.handleFileCat,
		common.ProcedureFileSearch:       d.handleFileSearch,
		common.ProcedureGitStatus:        d.handleGitStatus,
		common.ProcedureGitOriginal:      d.handleGitOriginal,
		common.ProcedureMachineInfo:      d.handleMachineInfo,
		common.ProcedureMachineIsDesktop: d.handleMachineIsDesktop,
		common.ProcedureProcessList:      d.handleProcessList,
		common.ProcedureProcessSignal:    d.handleProcessSignal,
		common.ProcedureAppList:          d.handleAppList,
		common.ProcedureAppIcon:          d.handleAppIcon,
		common.ProcedureAISessionList:    d.handleAISessionList,
		common.ProcedureAISessionPull:    d.handleAISessionPull,
		common.ProcedurePing: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult()
		},
	}

	// Pictures, Videos and Documents are listed and served while media-app is connected.
	handlers[common.ProcedureCapabilitiesList] = d.handleCapabilitiesList
	handlers[common.ProcedureAppsRegister] = d.handleAppsRegister

	// The wallpaper depends on a session D-Bus connection, which isn't available on a
	// headless server. Skip registering it there. Screen, MPRIS, audio, screenshot and
	// printer procedures are served by remotecontrol-app.
	if d.desktop && d.wallpaper != nil {
		maps.Copy(handlers, map[string]xconn.InvocationHandler{
			common.ProcedureWallpaperGet:      d.handleWallpaperGet,
			common.ProcedureWallpaperChecksum: d.handleWallpaperChecksum,
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

func (d *Deskconn) handleSessionLeave(event *xconn.Event) {
	sessionID, err := event.ArgUInt64(0)
	if err != nil {
		return
	}
	d.keys.delete(sessionID)
	d.handleAppUnregister(sessionID)
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

// handleIndexQuery serves Pictures, Videos and Documents: it decrypts the query and
// forwards it to media-app, which does the indexing.
func (d *Deskconn) handleIndexQuery(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
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

	resp := d.session.Call(common.ProcedureMediaIndexQuery).Args(plaintext).Do()
	if resp.Err != nil {
		var wampErr *xconn.Error
		if !errors.As(resp.Err, &wampErr) {
			return xconn.NewInvocationError(common.ErrOperationFailed, resp.Err.Error())
		}
		if wampErr.URI == wampproto.ErrNoSuchProcedure {
			return xconn.NewInvocationError(common.ErrOperationFailed, "media-app isn't running on this device")
		}
		// media-app's own errors, as they are.
		return xconn.NewInvocationError(wampErr.URI, wampErr.Args...)
	}
	resultBytes, err := resp.ArgBytes(0)
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
