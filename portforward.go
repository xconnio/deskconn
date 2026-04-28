package deskconn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedurePortForward = "io.xconn.deskconn.deskconnd.port.forward"

	msgConnect     = "C"
	msgKeyExchange = "K"
	msgClose       = "X"
)

func derivePortForwardKeys(privKey, peerPubKey []byte) ([]byte, []byte, error) {
	sharedSecret, err := PerformKeyExchange(privKey, peerPubKey)
	if err != nil {
		return nil, nil, err
	}
	b2f, err := DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return nil, nil, err
	}
	f2b, err := DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
	if err != nil {
		return nil, nil, err
	}
	return b2f, f2b, nil
}

type portForwardConn struct {
	conn       net.Conn
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
	sendKey    []byte
	receiveKey []byte
}

func newPortForwardConn(conn net.Conn, sendKey, receiveKey []byte) *portForwardConn {
	return &portForwardConn{
		conn:       conn,
		done:       make(chan struct{}),
		sendKey:    sendKey,
		receiveKey: receiveKey,
	}
}

// close signals the reader goroutine to stop and closes the underlying TCP connection.
func (c *portForwardConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

type portForwardSessions struct {
	conns map[uint64]map[uint64]*portForwardConn // callerID -> connID -> portForwardConn
	mu    sync.Mutex
}

func newPortForwardSessions() *portForwardSessions {
	return &portForwardSessions{
		conns: make(map[uint64]map[uint64]*portForwardConn),
	}
}

func (p *portForwardSessions) store(callerID, connID uint64, c *portForwardConn) {
	p.mu.Lock()
	if p.conns[callerID] == nil {
		p.conns[callerID] = make(map[uint64]*portForwardConn)
	}
	p.conns[callerID][connID] = c
	p.mu.Unlock()
}

func (p *portForwardSessions) fetch(callerID, connID uint64) (*portForwardConn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.conns[callerID]; ok {
		c, ok := m[connID]
		return c, ok
	}
	return nil, false
}

func (p *portForwardSessions) remove(callerID, connID uint64) {
	p.mu.Lock()
	if m, ok := p.conns[callerID]; ok {
		delete(m, connID)
		if len(m) == 0 {
			delete(p.conns, callerID)
		}
	}
	p.mu.Unlock()
}

func (p *portForwardSessions) deleteCaller(callerID uint64) {
	p.mu.Lock()
	if m, ok := p.conns[callerID]; ok {
		for _, c := range m {
			c.close()
		}
		delete(p.conns, callerID)
	}
	p.mu.Unlock()
}

func (d *Deskconn) handlePortForward(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	callerID := inv.Caller()

	if !inv.Progress() {
		connID, err := inv.ArgUInt64(0)
		if err == nil {
			if pf, ok := d.forwardSessions.fetch(callerID, connID); ok {
				pf.close()
				pf.wg.Wait()
			}
			d.forwardSessions.remove(callerID, connID)
		}
		return xconn.NewInvocationResult()
	}

	msgType, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}

	connID, err := inv.ArgUInt64(1)
	if err != nil {
		return xconn.NewInvocationError(xconn.ErrNoResult)
	}

	switch msgType {
	case msgConnect:
		host, err := inv.ArgString(2)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		port, err := inv.ArgString(3)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		clientPubKey, err := inv.ArgBytes(4)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		// Key exchange
		serverPubKey, serverPrivKey, err := CreateX25519KeyPair()
		if err != nil {
			_ = inv.SendProgress([]any{msgClose, connID}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		sendKey, receiveKey, err := derivePortForwardKeys(serverPrivKey, clientPubKey)
		if err != nil {
			_ = inv.SendProgress([]any{msgClose, connID}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		tcpConn, dialErr := net.Dial("tcp", net.JoinHostPort(host, port))
		if dialErr != nil {
			_ = inv.SendProgress([]any{msgClose, connID}, nil)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pf := newPortForwardConn(tcpConn, sendKey, receiveKey)
		d.forwardSessions.store(callerID, connID, pf)

		// Send the server public key so the client can complete key exchange.
		if err := inv.SendProgress([]any{msgKeyExchange, connID, serverPubKey}, nil); err != nil {
			pf.close()
			d.forwardSessions.remove(callerID, connID)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		pf.wg.Add(1)
		go func() {
			defer pf.wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, readErr := tcpConn.Read(buf)
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
						if sendErr := inv.SendProgress([]any{msgData, connID, encrypted}, nil); sendErr != nil {
							pf.close()
							return
						}
					}
				}
				if readErr != nil {
					select {
					case <-pf.done:
						// Closed by the !inv.Progress() handler.
					default:
						_ = inv.SendProgress([]any{msgClose, connID}, nil)
					}
					return
				}
			}
		}()

	case msgData:
		pf, ok := d.forwardSessions.fetch(callerID, connID)
		if !ok {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		encrypted, err := inv.ArgBytes(2)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		plaintext, err := DecryptPayload(encrypted, pf.receiveKey)
		if err != nil {
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}
		if _, writeErr := pf.conn.Write(plaintext); writeErr != nil {
			_ = pf.conn.Close()
		}

	case msgClose:
		if pf, ok := d.forwardSessions.fetch(callerID, connID); ok {
			pf.close()
			pf.wg.Wait()
		}
		d.forwardSessions.remove(callerID, connID)
	}

	return xconn.NewInvocationError(xconn.ErrNoResult)
}

func ForwardLocalPort(ctx context.Context, session *xconn.Session, remotePort, localPort string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%s", localPort))
	if err != nil {
		return fmt.Errorf("listen 127.0.0.1:%s: %w", localPort, err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var connCounter atomic.Uint64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		go forwardConnection(ctx, session, conn, "localhost", remotePort, &connCounter)
	}
}

func forwardConnection(ctx context.Context, session *xconn.Session, localConn net.Conn, remoteHost, remotePort string,
	counter *atomic.Uint64) {
	defer localConn.Close()

	connID := counter.Add(1)

	// Generate a per-connection X25519 keypair.
	clientPubKey, clientPrivKey, err := CreateX25519KeyPair()
	if err != nil {
		return
	}

	sendCh := make(chan []byte, 64)
	done := make(chan struct{})
	var doneOnce sync.Once

	closeDone := func() {
		doneOnce.Do(func() {
			close(done)
			localConn.Close()
		})
	}

	// keyReady is closed by the ProgressReceiver once the key exchange is done.
	// The ProgressSender blocks on it before sending any data so that no
	// plaintext is ever sent before the keys are established.
	keyReady := make(chan struct{})
	var sendKey, receiveKey []byte
	keyExchanged := false

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := localConn.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case sendCh <- chunk:
				case <-done:
					return
				}
			}
			if err != nil {
				closeDone()
				return
			}
		}
	}()

	firstSent := false
	callResp := session.Call(ProcedurePortForward).
		ProgressSender(func(_ context.Context) *xconn.Progress {
			// First message: include host, port, and client public key so the
			// server can perform key exchange before opening the TCP connection.
			if !firstSent {
				firstSent = true
				return &xconn.Progress{
					Args:    []any{msgConnect, connID, remoteHost, remotePort, clientPubKey},
					Options: map[string]any{"progress": true},
				}
			}

			// Block until key exchange completes before sending any data.
			if !keyExchanged {
				select {
				case <-keyReady:
					keyExchanged = true
				case <-done:
					return &xconn.Progress{Args: []any{connID}}
				case <-ctx.Done():
					closeDone()
					return &xconn.Progress{Args: []any{connID}}
				}
			}

			select {
			case <-ctx.Done():
				closeDone()
				return &xconn.Progress{Args: []any{connID}}
			case <-done:
				return &xconn.Progress{Args: []any{connID}}
			case chunk := <-sendCh:
				encrypted, encErr := EncryptPayload(chunk, sendKey)
				if encErr != nil {
					closeDone()
					return &xconn.Progress{Args: []any{connID}}
				}
				return &xconn.Progress{
					Args:    []any{msgData, connID, encrypted},
					Options: map[string]any{"progress": true},
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
			case msgKeyExchange:
				if len(result.Args()) < 3 {
					closeDone()
					return
				}
				serverPubKey, err := result.ArgBytes(2)
				if err != nil {
					closeDone()
					return
				}
				receiveKey, sendKey, err = derivePortForwardKeys(clientPrivKey, serverPubKey)
				if err != nil {
					closeDone()
					return
				}
				// Unblock the ProgressSender.
				close(keyReady)

			case msgData:
				if len(result.Args()) < 3 {
					return
				}
				encrypted, err := result.ArgBytes(2)
				if err != nil {
					return
				}
				plaintext, err := DecryptPayload(encrypted, receiveKey)
				if err != nil {
					return
				}
				_, _ = localConn.Write(plaintext)

			case msgClose:
				closeDone()
			}
		}).
		DoContext(ctx)

	_ = callResp.Err
	closeDone()
}
