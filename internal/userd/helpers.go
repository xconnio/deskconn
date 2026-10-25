package userd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

func parseFileProxyArgs(ctx context.Context, inv *xconn.Invocation, clientSessions *ClientSessions,
	cfgDirectory string) (strArg string, bytesArg []byte, sess *xconn.Session, invErr *xconn.Result) {
	realm, err := inv.ArgString(0)
	if err != nil {
		return "", nil, nil, xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	strArg, err = inv.ArgString(1)
	if err != nil {
		return "", nil, nil, xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	bytesArg, err = inv.ArgBytes(2)
	if err != nil {
		return "", nil, nil, xconn.NewResultError(common.ErrInvalidArgument, err.Error())
	}
	sess, err = clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
	if err != nil {
		return "", nil, nil, xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return strArg, bytesArg, sess, nil
}

// proxyKeyCache caches the client-role SessionKeys the proxy handlers derive per outbound
// machine session, so repeated proxied calls to the same machine don't re-run the key
// exchange every time. It's keyed by the session itself: session IDs aren't unique across
// machines. All the handlers share one (see ClientSessions), and calls at once share one
// exchange: the machine keeps a single set of keys per session, so each exchange replaces
// the keys of the one before.
type proxyKeyCache struct {
	mu         sync.Mutex
	keys       map[*xconn.Session]*common.SessionKeys
	exchanging map[*xconn.Session]chan struct{} // closed once the exchange under way is done
}

func newProxyKeyCache() *proxyKeyCache {
	return &proxyKeyCache{
		keys:       make(map[*xconn.Session]*common.SessionKeys),
		exchanging: make(map[*xconn.Session]chan struct{}),
	}
}

// keysFor returns session's keys, exchanging them first if there are none yet or the ones
// there are rejected: the machine refused them.
func (c *proxyKeyCache) keysFor(session *xconn.Session, rejected *common.SessionKeys) (*common.SessionKeys, error) {
	c.mu.Lock()
	for {
		if enc := c.keys[session]; enc != nil && enc != rejected {
			c.mu.Unlock()
			return enc, nil
		}
		exchanging, ok := c.exchanging[session]
		if !ok {
			break
		}
		c.mu.Unlock()
		<-exchanging
		c.mu.Lock()
	}
	exchanging := make(chan struct{})
	c.exchanging[session] = exchanging
	c.mu.Unlock()

	enc, err := common.ClientKeyExchange(session)

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.exchanging, session)
	close(exchanging)
	if err != nil {
		return nil, err
	}
	c.keys[session] = enc
	return enc, nil
}

// keysRejected reports whether the machine refused a call because it doesn't have the
// keys it was encrypted with, e.g. after another client's key exchange replaced them.
func keysRejected(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no session keys found") || strings.Contains(msg, "failed to decrypt")
}

func ProxyFileOpHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		procedure, payload, machineSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}
		realm, _ := inv.ArgString(0)
		defer clientSessions.holdSession(realm, machineSession)()

		result, err := clientSessions.keys.call(machineSession, procedure, payload)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewResult(result)
	}
}

// call makes an encrypted call to procedure on machineSession with the session's cached
// keys, exchanging keys first if there are none.
func (c *proxyKeyCache) call(machineSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, err := c.keysFor(machineSession, nil)
	if err != nil {
		return nil, err
	}
	result, err := common.EncryptedCall(machineSession, procedure, payload, enc)
	if err != nil && keysRejected(err) {
		// Keys the machine no longer has: exchange again and retry once. The machine
		// rejected the request before running it, so nothing is repeated.
		if enc, err = c.keysFor(machineSession, enc); err != nil {
			return nil, err
		}
		result, err = common.EncryptedCall(machineSession, procedure, payload, enc)
	}
	return result, err
}

func ProxyMachineInfoHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSess)()

		result, err := clientSessions.keys.call(machineSess, common.ProcedureMachineInfo, nil)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}

		return xconn.NewResult(result)
	}
}

func ProxyPingHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSess)()

		start := time.Now()
		callResp := machineSess.Call(common.ProcedurePing).Execute()
		if callResp.Err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewResult(time.Since(start).Milliseconds())
	}
}

func ProxyPrinterListHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSess)()

		var printers []any
		if err := appCall(machineSess, printerAppID, common.ProcedurePrinterList, nil, &printers); err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}

		return xconn.NewResult(printers)
	}
}

func ProxyPrinterPrintHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		printer, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		filename, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		data, err := inv.ArgBytes(3)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSess)()

		req := map[string]any{"printer": printer, "filename": filename, "data": data}
		var jobID string
		if err := appCall(machineSess, printerAppID, common.ProcedurePrinterPrint, req, &jobID); err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}

		return xconn.NewResult(jobID)
	}
}

// printerAppID is the app remotecontrol-app serves printing as.
const printerAppID = "printer"

// listedApp is an app as ProcedureCapabilitiesList lists it.
type listedApp struct {
	ID          string `json:"id"`
	Enabled     bool   `json:"enabled"`
	KeyExchange string `json:"key_exchange"`
}

// appCall calls one of app appID's procedures on a machine, end-to-end encrypted with the
// keys of the app's own key exchange (see ProcedureCapabilitiesList): req, if not nil, is
// sent as JSON and the JSON result is unmarshaled into out.
func appCall(session *xconn.Session, appID, procedure string, req, out any) error {
	listResp := session.Call(common.ProcedureCapabilitiesList).Execute()
	if listResp.Err != nil {
		return listResp.Err
	}
	if len(listResp.Args) == 0 {
		return errors.New("machine returned no apps list")
	}
	data, err := json.Marshal(listResp.Args[0])
	if err != nil {
		return err
	}
	var apps []listedApp
	if err := json.Unmarshal(data, &apps); err != nil {
		return err
	}
	i := slices.IndexFunc(apps, func(a listedApp) bool { return a.ID == appID })
	if i < 0 {
		return fmt.Errorf("%s isn't available on this machine", appID)
	}
	if !apps[i].Enabled {
		return fmt.Errorf("%s is disabled on this machine", appID)
	}

	enc, err := common.ClientKeyExchangeWith(session, apps[i].KeyExchange)
	if err != nil {
		return err
	}
	var payload []byte
	if req != nil {
		if payload, err = json.Marshal(req); err != nil {
			return err
		}
	}
	result, err := common.EncryptedCall(session, procedure, payload, enc)
	if err != nil {
		return err
	}
	return json.Unmarshal(result, out)
}

// ProxyCatHandler streams a file from realm's machine to the local CLI: it encrypts the
// path and decrypts the chunks with its cached keys for that machine, so they reach the
// CLI in plain text over the local socket.
func ProxyCatHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		remotePath, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		machineSession, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSession)()

		var rejected *common.SessionKeys
		for {
			enc, err := clientSessions.keys.keysFor(machineSession, rejected)
			if err != nil {
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}

			pathJSON, err := json.Marshal(remotePath)
			if err != nil {
				return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
			}
			encryptedPath, err := common.EncryptPayload(pathJSON, enc.SendKey)
			if err != nil {
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}

			err = forwardCat(ctx, machineSession, inv, encryptedPath, enc.ReceiveKey)
			if err != nil {
				if rejected == nil && keysRejected(err) {
					rejected = enc
					continue
				}
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}
			return xconn.NewResult()
		}
	}
}

func forwardCat(ctx context.Context, session *xconn.Session, inv *xconn.Invocation,
	encryptedPath, receiveKey []byte) error {
	call, err := session.Call(common.ProcedureFileCat).Args(encryptedPath).StreamContext(ctx)
	if err != nil {
		return err
	}
	defer call.Close()

	for {
		result, err := call.Receive(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if result.Details["progress"] != true {
			continue
		}
		encrypted, err := result.ArgBytes(0)
		if err != nil {
			return err
		}
		chunk, err := common.DecryptPayload(encrypted, receiveKey)
		if err != nil {
			return err
		}
		if err := inv.SendProgress([]any{chunk}, nil); err != nil {
			return err
		}
	}
}
