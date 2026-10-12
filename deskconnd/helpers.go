package deskconnd

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func parseFileProxyArgs(ctx context.Context, inv *xconn.Invocation, clientSessions *ClientSessions,
	cfgDirectory string) (strArg string, bytesArg []byte, sess *xconn.Session, invErr *xconn.InvocationResult) {
	realm, err := inv.ArgString(0)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	strArg, err = inv.ArgString(1)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	bytesArg, err = inv.ArgBytes(2)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	sess, err = clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return strArg, bytesArg, sess, nil
}

// proxyKeyCache caches the client-role SessionKeys ProxyFileOpHandler
// derives per outbound machine session, so repeated proxied calls to the
// same machine don't re-run the key exchange every time. It's keyed by the
// session itself: session IDs aren't unique across machines.
type proxyKeyCache struct {
	mu   sync.Mutex
	keys map[*xconn.Session]*common.SessionKeys
}

func newProxyKeyCache() *proxyKeyCache {
	return &proxyKeyCache{keys: make(map[*xconn.Session]*common.SessionKeys)}
}

func (c *proxyKeyCache) fetch(session *xconn.Session) (*common.SessionKeys, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	enc, ok := c.keys[session]
	return enc, ok
}

func (c *proxyKeyCache) store(session *xconn.Session, enc *common.SessionKeys) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[session] = enc
}

// keysRejected reports whether the machine refused a call because it doesn't have the
// keys it was encrypted with, e.g. after another client's key exchange replaced them.
func keysRejected(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no session keys found") || strings.Contains(msg, "failed to decrypt")
}

func ProxyFileOpHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newProxyKeyCache()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		procedure, payload, machineSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}
		realm, _ := inv.ArgString(0)
		defer clientSessions.holdSession(realm, machineSession)()

		result, err := km.call(machineSession, procedure, payload)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult(result)
	}
}

// call makes an encrypted call to procedure on machineSession with the session's cached
// keys, exchanging keys first if there are none.
func (c *proxyKeyCache) call(machineSession *xconn.Session, procedure string, payload []byte) ([]byte, error) {
	enc, cached := c.fetch(machineSession)
	for {
		if enc == nil {
			var err error
			enc, err = common.ClientKeyExchange(machineSession)
			if err != nil {
				return nil, err
			}
			c.store(machineSession, enc)
		}

		result, err := common.EncryptedCall(machineSession, procedure, payload, enc)
		if err == nil {
			return result, nil
		}
		// Cached keys the machine no longer has: exchange again and retry once. The
		// machine rejected the request before running it, so nothing is repeated.
		if cached && keysRejected(err) {
			enc, cached = nil, false
			continue
		}
		return nil, err
	}
}

func ProxyMachineInfoHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newProxyKeyCache()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		result, err := km.call(machineSess, common.ProcedureMachineInfo, nil)
		if err != nil {
			_ = machineSess.Leave()
			clientSessions.DeleteMachineSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		return xconn.NewInvocationResult(result)
	}
}

func ProxyPingHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		start := time.Now()
		callResp := machineSess.Call(common.ProcedurePing).Do()
		if callResp.Err != nil {
			_ = machineSess.Leave()
			clientSessions.DeleteMachineSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(time.Since(start).Milliseconds())
	}
}

func ProxyPrinterListHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		callResp := machineSess.Call(common.ProcedurePrinterList).Do()
		if callResp.Err != nil {
			_ = machineSess.Leave()
			clientSessions.DeleteMachineSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

func ProxyPrinterPrintHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		printer, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		filename, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		data, err := inv.ArgBytes(3)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		machineSess, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		callResp := machineSess.Call(common.ProcedurePrinterPrint).Args(printer, filename, data).Do()
		if callResp.Err != nil {
			_ = machineSess.Leave()
			clientSessions.DeleteMachineSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

// ProxyVPNStartHandler proxies "desk vpn start": it arms d to accept
// inbound VPN tunnel requests using helperSocket, a vpnd socket the
// caller already started.
//
// Returns as soon as arming succeeds, without blocking, so the CLI can hand
// off immediately. No tunnel can start at all until some caller has armed
// serving this way -- this feature's only gate today, in place of a real
// consent prompt.
func ProxyVPNStartHandler(d *Deskconn) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		helperSocket, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		helper, err := common.DialVPNHelper(helperSocket)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		if err := d.ArmVPNServing(helper); err != nil {
			_ = helper.Close()
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult()
	}
}

// ProxyVPNStopHandler proxies "desk vpn stop": disarms serving, tearing down any active
// tunnel and closing the helper connection so vpnd unwinds and exits. Meant to run as
// an independent command from "desk vpn start", not necessarily the same terminal.
func ProxyVPNStopHandler(d *Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		if !d.DisarmVPNServing() {
			return xconn.NewInvocationError(common.ErrOperationFailed, "not currently serving")
		}
		return xconn.NewInvocationResult()
	}
}

// ProxyCatHandler streams a file from realm's machine to the local CLI: it encrypts the
// path and decrypts the chunks with its cached keys for that machine, so they reach the
// CLI in plain text over the local socket.
func ProxyCatHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newProxyKeyCache()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		remotePath, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}
		machineSession, err := clientSessions.EnsureMachineSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		defer clientSessions.holdSession(realm, machineSession)()

		enc, cached := km.fetch(machineSession)
		for {
			if enc == nil {
				enc, err = common.ClientKeyExchange(machineSession)
				if err != nil {
					return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
				}
				km.store(machineSession, enc)
			}

			pathJSON, err := json.Marshal(remotePath)
			if err != nil {
				return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
			}
			encryptedPath, err := common.EncryptPayload(pathJSON, enc.SendKey)
			if err != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
			}

			callCtx, cancel := context.WithCancel(ctx)
			var chunkErr error
			callResp := machineSession.Call(common.ProcedureFileCat).
				ProgressReceiver(func(pr *xconn.ProgressResult) {
					if chunkErr != nil {
						return
					}
					encrypted, err := pr.ArgBytes(0)
					if err == nil {
						var chunk []byte
						if chunk, err = common.DecryptPayload(encrypted, enc.ReceiveKey); err == nil {
							err = inv.SendProgress([]any{chunk}, nil)
						}
					}
					if err != nil {
						chunkErr = err
						cancel()
					}
				}).
				Args(encryptedPath).
				DoContext(callCtx)
			cancel()

			if chunkErr != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, chunkErr.Error())
			}
			if callResp.Err != nil {
				// Cached keys the machine no longer has: it rejected the call before sending
				// anything, so exchange again and retry once.
				if cached && keysRejected(callResp.Err) {
					enc, cached = nil, false
					continue
				}
				return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
			}
			return xconn.NewInvocationResult()
		}
	}
}
