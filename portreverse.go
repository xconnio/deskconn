package deskconn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

const ProcedurePortReverse = "io.xconn.deskconn.deskconnd.port.reverse"

type reversePendingConn struct {
	conn          net.Conn
	serverPrivKey []byte
}

type portReverseCallerSession struct {
	ln      net.Listener
	pending map[uint64]*reversePendingConn
	active  map[uint64]*portForwardConn
	sync.Mutex
}

func newPortReverseCallerSession(ln net.Listener) *portReverseCallerSession {
	return &portReverseCallerSession{
		ln:      ln,
		pending: make(map[uint64]*reversePendingConn),
		active:  make(map[uint64]*portForwardConn),
	}
}

func (s *portReverseCallerSession) close() {
	s.ln.Close()
	s.Lock()
	for _, pf := range s.active {
		pf.close()
	}
	for _, pc := range s.pending {
		pc.conn.Close()
	}
	s.Unlock()
}

type portReverseSessions struct {
	sessions map[uint64]*portReverseCallerSession
	sync.Mutex
}

func newPortReverseSessions() *portReverseSessions {
	return &portReverseSessions{sessions: make(map[uint64]*portReverseCallerSession)}
}

func (p *portReverseSessions) store(callerID uint64, ln net.Listener) {
	p.Lock()
	p.sessions[callerID] = newPortReverseCallerSession(ln)
	p.Unlock()
}

func (p *portReverseSessions) storePending(callerID, connID uint64, conn net.Conn, serverPrivKey []byte) {
	p.Lock()
	s, ok := p.sessions[callerID]
	p.Unlock()
	if !ok {
		return
	}
	s.Lock()
	s.pending[connID] = &reversePendingConn{conn: conn, serverPrivKey: serverPrivKey}
	s.Unlock()
}

func (p *portReverseSessions) fetchPending(callerID, connID uint64) (*reversePendingConn, bool) {
	p.Lock()
	s, ok := p.sessions[callerID]
	p.Unlock()
	if !ok {
		return nil, false
	}
	s.Lock()
	defer s.Unlock()
	pc, ok := s.pending[connID]
	return pc, ok
}

func (p *portReverseSessions) activateConn(callerID, connID uint64, pf *portForwardConn) {
	p.Lock()
	s, ok := p.sessions[callerID]
	p.Unlock()
	if !ok {
		return
	}
	s.Lock()
	delete(s.pending, connID)
	s.active[connID] = pf
	s.Unlock()
}

func (p *portReverseSessions) fetchActive(callerID, connID uint64) (*portForwardConn, bool) {
	p.Lock()
	s, ok := p.sessions[callerID]
	p.Unlock()
	if !ok {
		return nil, false
	}
	s.Lock()
	defer s.Unlock()
	pf, ok := s.active[connID]
	return pf, ok
}

func (p *portReverseSessions) removeActive(callerID, connID uint64) {
	p.Lock()
	s, ok := p.sessions[callerID]
	p.Unlock()
	if !ok {
		return
	}
	s.Lock()
	delete(s.active, connID)
	s.Unlock()
}

func (p *portReverseSessions) stop(callerID uint64) {
	p.Lock()
	s, ok := p.sessions[callerID]
	if ok {
		delete(p.sessions, callerID)
	}
	p.Unlock()
	if ok {
		s.close()
	}
}

func (d *Deskconn) handlePortReverse(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	callerID := inv.Caller()

	if !inv.Progress() {
		d.reverseSessions.stop(callerID)
		return xconn.NewInvocationResult()
	}

	msgType, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}

	switch msgType {
	case msgConnect:
		remotePort, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		ln, listenErr := net.Listen("tcp", fmt.Sprintf(":%s", remotePort))
		if listenErr != nil {
			_ = inv.SendProgress([]any{msgClose, uint64(0)}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		d.reverseSessions.store(callerID, ln)

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

				d.reverseSessions.storePending(callerID, connID, conn, serverPrivKey)

				if sendErr := inv.SendProgress([]any{msgConnect, connID, serverPubKey}, nil); sendErr != nil {
					conn.Close()
					d.reverseSessions.stop(callerID)
					return
				}
			}
		}()

	case msgKeyExchange:
		connID, err := inv.ArgUInt64(1)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		clientPubKey, err := inv.ArgBytes(2)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pending, ok := d.reverseSessions.fetchPending(callerID, connID)
		if !ok {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		sendKey, receiveKey, kErr := derivePortForwardKeys(pending.serverPrivKey, clientPubKey)
		if kErr != nil {
			pending.conn.Close()
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pf := newPortForwardConn(pending.conn, sendKey, receiveKey)
		d.reverseSessions.activateConn(callerID, connID, pf)

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
						if sendErr := inv.SendProgress([]any{msgData, connID, seq, encrypted}, nil); sendErr != nil {
							pf.close()
							return
						}
					}
				}
				if readErr != nil {
					select {
					case <-pf.done:
					default:
						_ = inv.SendProgress([]any{msgClose, connID}, nil)
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
		pf, ok := d.reverseSessions.fetchActive(callerID, connID)
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
		if pf, ok := d.reverseSessions.fetchActive(callerID, connID); ok {
			pf.close()
			pf.wg.Wait()
		}
		d.reverseSessions.removeActive(callerID, connID)
	}

	return xconn.NewInvocationError(xconn.ErrNoResult)
}

// ReverseLocalPort calls ProcedurePortReverse on the remote device, which listens on
// remotePort. Incoming connections on the remote are forwarded to localhost:localPort.
func ReverseLocalPort(ctx context.Context, session *xconn.Session, remotePort, localPort string) error {
	type outMsg struct {
		args []any
	}

	outCh := make(chan outMsg, 64)
	done := make(chan struct{})
	var doneOnce sync.Once

	closeDone := func() {
		doneOnce.Do(func() { close(done) })
	}

	var (
		mu         sync.Mutex
		localConns = make(map[uint64]*portForwardConn)
	)

	firstSent := false
	callResp := session.Call(ProcedurePortReverse).
		ProgressSender(func(_ context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				return &xconn.Progress{
					Args:    []any{msgConnect, remotePort},
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
					Args:    msg.args,
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
			case msgConnect:
				connID, err := result.ArgUInt64(1)
				if err != nil {
					return
				}
				serverPubKey, err := result.ArgBytes(2)
				if err != nil {
					return
				}

				localConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", localPort))
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
