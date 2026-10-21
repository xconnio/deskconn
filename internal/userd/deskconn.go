package userd

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const MetaTopicSessionLeave = "wamp.session.on_leave"

type Deskconn struct {
	shellSession         *interactiveShellSession
	keys                 *keyManager
	agentForwardSessions *agentForwardSessions
	files                *FileBrowser
	thumbnails           *thumbnailCache
	processes            *ProcessMonitor
	appRegistry          *AppRegistry
	desktop              bool
	vpn                  *vpnServer
	capabilities         *capabilities
	relayConns           relayConns
}

// NewDeskconn returns the machine's APIs.
func NewDeskconn(desktopEnvironment bool, cfgDirectory string) *Deskconn {
	d := &Deskconn{
		shellSession:         newInteractiveShellSession(),
		keys:                 newKeyManager(),
		agentForwardSessions: newAgentForwardSessions(),
		files:                NewFileBrowser(),
		thumbnails:           newThumbnailCache(cfgDirectory),
		processes:            NewProcessMonitor(),
		appRegistry:          NewAppRegistry(),
		desktop:              desktopEnvironment,
		vpn:                  newVPNServer(),
	}
	d.shellSession.agentForward = d.agentForwardSessions
	d.capabilities = newCapabilities(d.apps(), cfgDirectory)
	common.SafeGo(d.thumbnails.sweepLoop)
	return d
}

func (d *Deskconn) Register(session *xconn.Session) error {
	handlers := map[string]xconn.InvocationHandler{
		common.ProcedureKeyExchange:      d.handleKeyExchange,
		common.ProcedureMachineInfo:      d.handleMachineInfo,
		common.ProcedureMachineIsDesktop: d.handleMachineIsDesktop,
		common.ProcedureCapabilitiesList: d.capabilities.handleList,
		common.ProcedureCapabilitiesSet:  d.capabilities.handleSet,
		common.ProcedureAppsRegister:     d.handleAppsRegister,
		common.ProcedurePing: func(_ context.Context, _ *xconn.Invocation) *xconn.InvocationResult {
			return xconn.NewInvocationResult()
		},
	}

	// Screen, wallpaper, MPRIS, audio, screenshot and printer procedures are served by
	// remotecontrol-app.

	for uri, handler := range handlers {
		response := session.Register(uri, handler).Invoke(wampproto.InvokeLast).Do()
		if response.Err != nil {
			return response.Err
		}

		log.Printf("Registered procedure %s", uri)
	}

	// Each app's procedures, registered while that app is enabled (see Deskconn.apps).
	if err := d.capabilities.register(session); err != nil {
		return err
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
	d.thumbnails.prefetch(inv.Caller(), result.Entries, args.Cursor == "")

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
