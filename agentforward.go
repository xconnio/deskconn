package deskconn

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

// msgReady announces that the remote listener is up and reports its socket path. It is
// distinct from msgConnect (which always carries a connID + pubkey, a different arg shape)
// so the client can tell the one-time setup ack apart from per-connection accept notices.
const msgReady = "R"

// agentForwardCallerSession owns one forwarding listener. inv is the invocation currently
// used to push accept/data notifications to the caller; it's swapped in place by takeOver
// when the underlying device connection migrates (e.g. routed mode's QUIC-to-WebRTC upgrade),
// so the listener and its socket path never need to change — only who they talk through does.
type agentForwardCallerSession struct {
	inv             *xconn.Invocation
	currentCallerID uint64
	ln              net.Listener
	sockPath        string
	dir             string
	pending         map[uint64]*reversePendingConn
	active          map[uint64]*portForwardConn
	sync.Mutex
}

func newAgentForwardCallerSession(inv *xconn.Invocation, ln net.Listener,
	sockPath, dir string) *agentForwardCallerSession {
	return &agentForwardCallerSession{
		inv:             inv,
		currentCallerID: inv.Caller(),
		ln:              ln,
		sockPath:        sockPath,
		dir:             dir,
		pending:         make(map[uint64]*reversePendingConn),
		active:          make(map[uint64]*portForwardConn),
	}
}

func (s *agentForwardCallerSession) currentInv() *xconn.Invocation {
	s.Lock()
	defer s.Unlock()
	return s.inv
}

// takeOver switches the session to a freshly arrived invocation from the same authenticated
// caller, so an in-flight or already-completed migration keeps using the existing listener.
func (s *agentForwardCallerSession) takeOver(inv *xconn.Invocation) {
	s.Lock()
	s.inv = inv
	s.currentCallerID = inv.Caller()
	s.Unlock()
}

// owns reports whether callerID is the invocation currently responsible for this session —
// used to tell a stale (superseded) call ending from a real shutdown.
func (s *agentForwardCallerSession) owns(callerID uint64) bool {
	s.Lock()
	defer s.Unlock()
	return s.currentCallerID == callerID
}

func (s *agentForwardCallerSession) close() {
	s.ln.Close()
	s.Lock()
	for _, pf := range s.active {
		pf.close()
	}
	for _, pc := range s.pending {
		pc.conn.Close()
	}
	s.Unlock()
	_ = os.RemoveAll(s.dir)
}

type agentForwardSessions struct {
	sessions map[string]*agentForwardCallerSession // keyed by CallerAuthID
	sync.Mutex
}

func newAgentForwardSessions() *agentForwardSessions {
	return &agentForwardSessions{sessions: make(map[string]*agentForwardCallerSession)}
}

func (a *agentForwardSessions) store(authID string, s *agentForwardCallerSession) {
	a.Lock()
	a.sessions[authID] = s
	a.Unlock()
}

func (a *agentForwardSessions) fetch(authID string) (*agentForwardCallerSession, bool) {
	a.Lock()
	defer a.Unlock()
	s, ok := a.sessions[authID]
	return s, ok
}

func (a *agentForwardSessions) storePending(authID string, connID uint64, conn net.Conn, serverPrivKey []byte) {
	s, ok := a.fetch(authID)
	if !ok {
		return
	}
	s.Lock()
	s.pending[connID] = &reversePendingConn{conn: conn, serverPrivKey: serverPrivKey}
	s.Unlock()
}

func (a *agentForwardSessions) fetchPending(authID string, connID uint64) (*reversePendingConn, bool) {
	s, ok := a.fetch(authID)
	if !ok {
		return nil, false
	}
	s.Lock()
	defer s.Unlock()
	pc, ok := s.pending[connID]
	return pc, ok
}

func (a *agentForwardSessions) activateConn(authID string, connID uint64, pf *portForwardConn) {
	s, ok := a.fetch(authID)
	if !ok {
		return
	}
	s.Lock()
	delete(s.pending, connID)
	s.active[connID] = pf
	s.Unlock()
}

func (a *agentForwardSessions) fetchActive(authID string, connID uint64) (*portForwardConn, bool) {
	s, ok := a.fetch(authID)
	if !ok {
		return nil, false
	}
	s.Lock()
	defer s.Unlock()
	pf, ok := s.active[connID]
	return pf, ok
}

func (a *agentForwardSessions) removeActive(authID string, connID uint64) {
	s, ok := a.fetch(authID)
	if !ok {
		return
	}
	s.Lock()
	delete(s.active, connID)
	s.Unlock()
}

// stop tears down whichever session is currently owned by callerID, if any. It's a no-op when
// callerID belonged to a session that has since migrated to a different invocation.
func (a *agentForwardSessions) stop(callerID uint64) {
	a.Lock()
	var authID string
	var s *agentForwardCallerSession
	for id, sess := range a.sessions {
		if sess.owns(callerID) {
			authID, s = id, sess
			break
		}
	}
	if s != nil {
		delete(a.sessions, authID)
	}
	a.Unlock()
	if s != nil {
		s.close()
	}
}

// socketPath returns the forwarded agent socket path currently owned by callerID, if agent
// forwarding is active for it. Used when spawning a new PTY for that caller to set
// SSH_AUTH_SOCK.
func (a *agentForwardSessions) socketPath(callerID uint64) (string, bool) {
	a.Lock()
	defer a.Unlock()
	for _, sess := range a.sessions {
		if sess.owns(callerID) {
			return sess.sockPath, true
		}
	}
	return "", false
}

// handleAgentForward implements ProcedureAgentForward: on the first progressive message it
// creates a private UNIX socket (0700 dir, 0600 socket, matching ssh-agent's own default
// permissions) and listens on it, reporting the path back via msgReady. Every connection
// accepted on that socket is then relayed back to the caller over the same progressive call.
func (d *Deskconn) handleAgentForward(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	callerID := inv.Caller()

	if !inv.Progress() {
		d.agentForwardSessions.stop(callerID)
		return xconn.NewInvocationResult()
	}

	authID := inv.CallerAuthID()

	msgType, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}

	switch msgType {
	case msgConnect:
		// A forwarding session already exists for this identity: this is a migration (or a
		// reconnect) arriving over a new transport, not a fresh request. Reuse the existing
		// listener/socket instead of creating a new one — the shell's SSH_AUTH_SOCK was set
		// from the original path at PTY-spawn time and can never be changed afterward, so a
		// new path here would silently orphan agent forwarding for the rest of the session.
		if existing, ok := d.agentForwardSessions.fetch(authID); ok {
			existing.takeOver(inv)
			if sendErr := inv.SendProgress([]any{msgReady, existing.sockPath}, nil); sendErr != nil {
				d.agentForwardSessions.stop(inv.Caller())
			}
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		dir, dirErr := os.MkdirTemp("", "deskconn-agent-*")
		if dirErr != nil {
			_ = inv.SendProgress([]any{msgClose, uint64(0)}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		if chmodErr := os.Chmod(dir, 0700); chmodErr != nil { // nolint: gosec
			_ = os.RemoveAll(dir)
			_ = inv.SendProgress([]any{msgClose, uint64(0)}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		sockPath := filepath.Join(dir, "agent.sock")
		ln, listenErr := net.Listen("unix", sockPath)
		if listenErr != nil {
			_ = os.RemoveAll(dir)
			_ = inv.SendProgress([]any{msgClose, uint64(0)}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		if chmodErr := os.Chmod(sockPath, 0600); chmodErr != nil { // nolint: gosec
			_ = ln.Close()
			_ = os.RemoveAll(dir)
			_ = inv.SendProgress([]any{msgClose, uint64(0)}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		session := newAgentForwardCallerSession(inv, ln, sockPath, dir)
		d.agentForwardSessions.store(authID, session)

		if sendErr := inv.SendProgress([]any{msgReady, sockPath}, nil); sendErr != nil {
			d.agentForwardSessions.stop(callerID)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		var connCounter atomic.Uint64
		go func() {
			for {
				conn, acceptErr := ln.Accept()
				if acceptErr != nil {
					return
				}
				connID := connCounter.Add(1)

				serverPubKey, serverPrivKey, kErr := CreateX25519KeyPair()
				if kErr != nil {
					conn.Close()
					continue
				}

				d.agentForwardSessions.storePending(authID, connID, conn, serverPrivKey)

				curInv := session.currentInv()
				if sendErr := curInv.SendProgress([]any{msgConnect, connID, serverPubKey}, nil); sendErr != nil {
					// The invocation we captured may be stale mid-migration; drop this one
					// accept attempt but keep the listener alive for the next one instead of
					// tearing down the whole session on a single send failure.
					conn.Close()
					continue
				}
			}
		}()

	case msgKeyExchange:
		session, ok := d.agentForwardSessions.fetch(authID)
		if !ok {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		connID, err := inv.ArgUInt64(1)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		clientPubKey, err := inv.ArgBytes(2)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pending, ok := d.agentForwardSessions.fetchPending(authID, connID)
		if !ok {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		sendKey, receiveKey, kErr := derivePortForwardKeys(pending.serverPrivKey, clientPubKey)
		if kErr != nil {
			pending.conn.Close()
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pf := newPortForwardConn(pending.conn, sendKey, receiveKey)
		d.agentForwardSessions.activateConn(authID, connID, pf)

		pf.wg.Add(1)
		go func() {
			defer pf.wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, readErr := pf.conn.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					select {
					case <-pf.done:
						return
					default:
						encrypted, encErr := EncryptPayload(chunk, pf.sendKey)
						if encErr != nil {
							pf.close()
							return
						}
						seq := pf.nextSendSeq()
						curInv := session.currentInv()
						if sendErr := curInv.SendProgress([]any{msgData, connID, seq, encrypted}, nil); sendErr != nil {
							pf.close()
							return
						}
					}
				}
				if readErr != nil {
					select {
					case <-pf.done:
					default:
						_ = session.currentInv().SendProgress([]any{msgClose, connID}, nil)
					}
					return
				}
			}
		}()

	case msgData:
		connID, err := inv.ArgUInt64(1)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		pf, ok := d.agentForwardSessions.fetchActive(authID, connID)
		if !ok {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		seq, err := inv.ArgUInt64(2)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		encrypted, err := inv.ArgBytes(3)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		plaintext, err := DecryptPayload(encrypted, pf.receiveKey)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		if writeErr := pf.deliver(seq, plaintext); writeErr != nil {
			_ = pf.conn.Close()
		}

	case msgClose:
		connID, err := inv.ArgUInt64(1)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		if pf, ok := d.agentForwardSessions.fetchActive(authID, connID); ok {
			pf.close()
			pf.wg.Wait()
		}
		d.agentForwardSessions.removeActive(authID, connID)
	}

	return xconn.NewInvocationError(xconn.ErrNoResult)
}

// ProxyAgentForwardHandler proxies ProcedureAgentForward with transparent migration from QUIC
// to WebRTC, mirroring ProxyShellHandler's structure (helpers.go): it starts on a QUIC device
// session for a fast start, then re-issues the call on the WebRTC session once the background
// upgrade completes, relaying everything back to the caller through the same local invocation
// throughout so neither the CLI nor the caller's SSH_AUTH_SOCK ever needs to change.
func ProxyAgentForwardHandler(proxyCalls *ProxyCalls, clientSessions *ClientSessions,
	cfgDirectory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		caller := inv.Caller()

		if !inv.Progress() {
			if proxyCall, exists := proxyCalls.Fetch(caller); exists {
				if len(inv.Args()) > 0 {
					proxyCall.send(xconn.NewFinalProgress(inv.Args()...))
				}
				proxyCall.closeChannel()
				proxyCalls.Delete(caller)
			}
			return xconn.NewInvocationResult()
		}

		var resultCh chan *xconn.InvocationResult
		migrationCtx, cancelMigration := context.WithCancel(ctx)
		defer cancelMigration()

		proxyCall, exists := proxyCalls.Fetch(caller)
		if !exists {
			proxyCall = newProxyCall()
			proxyCalls.Store(caller, proxyCall)

			realm, err := inv.ArgString(0)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}
			initialArgs := append([]any(nil), inv.Args()[1:]...)

			deviceSession, upgradeCh, err := clientSessions.EnsureDeviceSessionWithUpgrade(ctx, realm, cfgDirectory)
			if err != nil {
				return xconn.NewInvocationError(ErrOperationFailed, err.Error())
			}

			var migratedMu sync.Mutex
			migrated := false
			migrationStarted := false

			resultCh = make(chan *xconn.InvocationResult, 1)
			cloudCh := proxyCall.progressChan

			var closeCloudOnce sync.Once
			closeCloud := func() {
				closeCloudOnce.Do(func() {
					close(cloudCh)
				})
			}
			proxyCall.setCloseFunc(closeCloud)

			go func() {
				callResp := deviceSession.Call(ProcedureAgentForward).
					ProgressSender(func(ctx context.Context) *xconn.Progress {
						p, ok := <-cloudCh
						if !ok {
							return xconn.NewFinalProgress()
						}
						return p
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						migratedMu.Lock()
						m := migrated
						started := migrationStarted
						migratedMu.Unlock()
						if !m && !started {
							_ = inv.SendProgress(pr.Args(), nil)
						}
					}).Do()

				migratedMu.Lock()
				m := migrated
				migratedMu.Unlock()
				if !m {
					if callResp.Err != nil {
						select {
						case resultCh <- xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error()):
						default:
						}
						return
					}
					select {
					case resultCh <- xconn.NewInvocationResult():
					default:
					}
				}
			}()

			// Migration goroutine: fires when QUIC upgrades to WebRTC in the background.
			go func() { //nolint:gosec
				var webrtcSession *xconn.Session
				select {
				case ws, ok := <-upgradeCh:
					if !ok || ws == nil {
						return // upgrade failed or already P2P; QUIC goroutine owns the result
					}
					webrtcSession = ws
				case <-migrationCtx.Done():
					return
				}

				newCh := make(chan *xconn.Progress, 32)
				switched := false

				migratedMu.Lock()
				migrationStarted = true
				migratedMu.Unlock()

				webrtcCallCtx, ok := clientSessions.SessionContext(realm)
				if !ok {
					webrtcCallCtx = context.Background()
				}

				callResp := webrtcSession.Call(ProcedureAgentForward).
					ProgressSender(func(_ context.Context) *xconn.Progress {
						if !switched {
							switched = true
							return xconn.NewProgress(initialArgs...)
						}
						select {
						case p, ok := <-newCh:
							if !ok {
								return xconn.NewFinalProgress()
							}
							return p
						case <-migrationCtx.Done():
							return xconn.NewFinalProgress()
						}
					}).
					ProgressReceiver(func(pr *xconn.ProgressResult) {
						migratedMu.Lock()
						if !migrated {
							migrated = true
							proxyCall.switchChannel(newCh)
							proxyCall.setCloseFunc(nil)
							closeCloud()
						}
						migratedMu.Unlock()
						_ = inv.SendProgress(pr.Args(), nil)
					}).DoContext(webrtcCallCtx) //nolint:contextcheck

				closeCloud()
				if callResp.Err != nil {
					select {
					case resultCh <- xconn.NewInvocationError(ErrOperationFailed, callResp.Err.Error()):
					default:
					}
					clientSessions.DeleteDeviceSession(realm)
					return
				}
				select {
				case resultCh <- xconn.NewInvocationResult():
				default:
				}
			}()
		}

		if len(inv.Args()) > 2 {
			proxyCall.send(xconn.NewProgress(inv.Args()[1:]...))
		} else {
			payload, err := inv.ArgBytes(1)
			if err != nil {
				return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
			}
			proxyCall.send(xconn.NewProgress(payload))
		}
		if resultCh != nil {
			return <-resultCh
		}
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}
}

func RunAgentForward(ctx context.Context, session *xconn.Session, realm, localSockPath string,
	ready chan<- error) error {
	procedure := ProcedureAgentForward
	if realm != "" {
		procedure = ProcedureProxyAgentForward
	}

	type outMsg struct {
		args []any
	}

	outCh := make(chan outMsg, 64)
	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() {
		doneOnce.Do(func() { close(done) })
	}

	var readyOnce sync.Once
	signalReady := func(err error) {
		readyOnce.Do(func() { ready <- err })
	}

	var (
		mu         sync.Mutex
		localConns = make(map[uint64]*portForwardConn)
	)

	withRealmPrefix := func(args []any) []any {
		if realm == "" {
			return args
		}
		return append([]any{realm}, args...)
	}

	firstSent := false
	callResp := session.Call(procedure).
		ProgressSender(func(_ context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				return &xconn.Progress{
					Args:    withRealmPrefix([]any{msgConnect, uint64(0)}),
					Options: map[string]any{wampproto.OptionProgress: true},
				}
			}
			select {
			case <-ctx.Done():
				closeDone()
				return &xconn.Progress{Args: []any{}}
			case <-done:
				return &xconn.Progress{Args: []any{}}
			case msg := <-outCh:
				return &xconn.Progress{
					Args:    withRealmPrefix(msg.args),
					Options: map[string]any{wampproto.OptionProgress: true},
				}
			}
		}).
		ProgressReceiver(func(result *xconn.ProgressResult) {
			if len(result.Args()) == 0 {
				closeDone()
				return
			}
			msgType, err := result.ArgString(0)
			if err != nil {
				return
			}

			switch msgType {
			case msgReady:
				signalReady(nil)

			case msgConnect:
				connID, err := result.ArgUInt64(1)
				if err != nil {
					return
				}
				serverPubKey, err := result.ArgBytes(2)
				if err != nil {
					return
				}

				localConn, dialErr := net.Dial("unix", localSockPath)
				if dialErr != nil {
					select {
					case outCh <- outMsg{[]any{msgClose, connID}}:
					case <-done:
					}
					return
				}

				clientPubKey, clientPrivKey, kErr := CreateX25519KeyPair()
				if kErr != nil {
					localConn.Close()
					select {
					case outCh <- outMsg{[]any{msgClose, connID}}:
					case <-done:
					}
					return
				}

				receiveKey, sendKey, kErr := derivePortForwardKeys(clientPrivKey, serverPubKey)
				if kErr != nil {
					localConn.Close()
					select {
					case outCh <- outMsg{[]any{msgClose, connID}}:
					case <-done:
					}
					return
				}

				pf := newPortForwardConn(localConn, sendKey, receiveKey)
				mu.Lock()
				localConns[connID] = pf
				mu.Unlock()

				select {
				case outCh <- outMsg{[]any{msgKeyExchange, connID, clientPubKey}}:
				case <-done:
					pf.close()
					return
				}

				pf.wg.Add(1)
				go func() {
					defer pf.wg.Done()
					defer func() {
						mu.Lock()
						delete(localConns, connID)
						mu.Unlock()
					}()
					buf := make([]byte, 32*1024)
					for {
						n, readErr := localConn.Read(buf)
						if n > 0 {
							chunk := make([]byte, n)
							copy(chunk, buf[:n])
							select {
							case <-pf.done:
								return
							default:
								encrypted, encErr := EncryptPayload(chunk, pf.sendKey)
								if encErr != nil {
									pf.close()
									return
								}
								seq := pf.nextSendSeq()
								select {
								case outCh <- outMsg{[]any{msgData, connID, seq, encrypted}}:
								case <-done:
									return
								case <-pf.done:
									return
								}
							}
						}
						if readErr != nil {
							select {
							case <-pf.done:
							default:
								select {
								case outCh <- outMsg{[]any{msgClose, connID}}:
								case <-done:
								}
							}
							return
						}
					}
				}()

			case msgData:
				connID, err := result.ArgUInt64(1)
				if err != nil {
					return
				}
				mu.Lock()
				pf, ok := localConns[connID]
				mu.Unlock()
				if !ok {
					return
				}
				seq, err := result.ArgUInt64(2)
				if err != nil {
					return
				}
				encrypted, err := result.ArgBytes(3)
				if err != nil {
					return
				}
				plaintext, err := DecryptPayload(encrypted, pf.receiveKey)
				if err != nil {
					return
				}
				_ = pf.deliver(seq, plaintext)

			case msgClose:
				connID, err := result.ArgUInt64(1)
				if err != nil {
					return
				}
				if connID == 0 {
					// Sentinel from handleAgentForward: setup failed before a
					// listener ever came up (e.g. couldn't bind the socket).
					signalReady(fmt.Errorf("remote device failed to start agent forwarding"))
					closeDone()
					return
				}
				mu.Lock()
				pf, ok := localConns[connID]
				mu.Unlock()
				if ok {
					pf.close()
					pf.wg.Wait()
					mu.Lock()
					delete(localConns, connID)
					mu.Unlock()
				}
			}
		}).
		DoContext(ctx)

	closeDone()
	switch {
	case callResp.Err != nil:
		signalReady(callResp.Err)
	case ctx.Err() != nil:
		signalReady(ctx.Err())
	default:
		signalReady(fmt.Errorf("agent forwarding call ended before becoming ready"))
	}

	mu.Lock()
	for _, pf := range localConns {
		pf.close()
	}
	mu.Unlock()

	if callResp.Err != nil {
		return callResp.Err
	}
	return ctx.Err()
}
