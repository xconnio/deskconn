package deskconnd

import (
	"context"
	"sync"
	"time"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/iptun"
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
	sess, err = clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
	if err != nil {
		return "", nil, nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return strArg, bytesArg, sess, nil
}

// proxyKeyCache caches the client-role SessionKeys ProxyFileOpHandler
// derives per outbound device session, so repeated proxied calls to the
// same device don't re-run the key exchange every time.
type proxyKeyCache struct {
	mu   sync.Mutex
	keys map[uint64]*common.SessionKeys
}

func newProxyKeyCache() *proxyKeyCache {
	return &proxyKeyCache{keys: make(map[uint64]*common.SessionKeys)}
}

func (c *proxyKeyCache) fetch(sessionID uint64) (*common.SessionKeys, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	enc, ok := c.keys[sessionID]
	return enc, ok
}

func (c *proxyKeyCache) store(sessionID uint64, enc *common.SessionKeys) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[sessionID] = enc
}

func ProxyFileOpHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	km := newProxyKeyCache()

	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		procedure, payload, deviceSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}

		enc, ok := km.fetch(deviceSession.ID())
		if !ok {
			var err error
			enc, err = common.ClientKeyExchange(deviceSession)
			if err != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
			}
			km.store(deviceSession.ID(), enc)
		}

		result, err := common.EncryptedCall(deviceSession, procedure, payload, enc)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewInvocationResult(result)
	}
}

func ProxyDeviceInfoHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(common.ProcedureDeviceInfo).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

func ProxyPingHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
		}

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		start := time.Now()
		callResp := deviceSess.Call(common.ProcedurePing).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
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

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(common.ProcedurePrinterList).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
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

		deviceSess, err := clientSessions.EnsureDeviceSession(ctx, realm, cfgDirectory)
		if err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}

		callResp := deviceSess.Call(common.ProcedurePrinterPrint).Args(printer, filename, data).Do()
		if callResp.Err != nil {
			_ = deviceSess.Leave()
			clientSessions.DeleteDeviceSession(realm)
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}

		return xconn.NewInvocationResult(callResp.Args()...)
	}
}

// ProxyVPNStartHandler proxies "deskconn vpn start": it arms d to accept
// inbound VPN tunnel requests using helperSocket, a deskconn-vpnd socket the
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

		helper, err := iptun.DialClient(helperSocket)
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

// ProxyVPNStopHandler proxies "deskconn vpn stop": disarms serving, tearing down any active
// tunnel and closing the helper connection so deskconn-vpnd unwinds and exits. Meant to run as
// an independent command from "deskconn vpn start", not necessarily the same terminal.
func ProxyVPNStopHandler(d *Deskconn) xconn.InvocationHandler {
	return func(context.Context, *xconn.Invocation) *xconn.InvocationResult {
		if !d.DisarmVPNServing() {
			return xconn.NewInvocationError(common.ErrOperationFailed, "not currently serving")
		}
		return xconn.NewInvocationResult()
	}
}

func ProxyCatHandler(clientSessions *ClientSessions, cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		remotePath, publicKey, deviceSession, invErr := parseFileProxyArgs(ctx, inv, clientSessions, cfgDirectory)
		if invErr != nil {
			return invErr
		}

		callResp := deviceSession.Call(common.ProcedureFileCat).
			ProgressReceiver(func(pr *xconn.ProgressResult) {
				_ = inv.SendProgress(pr.Args(), nil)
			}).
			Args(remotePath, publicKey).
			DoContext(ctx)

		if callResp.Err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, callResp.Err.Error())
		}
		return xconn.NewInvocationResult()
	}
}
