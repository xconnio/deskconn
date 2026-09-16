package deskconn

import (
	"net"
	"sync"
	"sync/atomic"
)

// This file holds the low-level per-connection relay primitives shared by
// agent forwarding (agentforward.go), which still speaks this WAMP-based
// connID/msgConnect/msgKeyExchange/msgClose/msgData protocol. `port
// forward`/`port reverse` themselves no longer use any of it -- they moved
// to the raw-stream protocol in portstream.go/portstreamclient.go, which
// needs none of this (no WAMP call to multiplex over, so no connID/seq
// bookkeeping at all; see portstream.go's doc comments for why).
const (
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

// reversePendingConn is an accepted connection awaiting its key exchange
// message before it can be activated into a portForwardConn.
type reversePendingConn struct {
	conn          net.Conn
	serverPrivKey []byte
}

type portForwardConn struct {
	conn       net.Conn
	done       chan struct{}
	once       sync.Once
	wg         sync.WaitGroup
	sendKey    []byte
	receiveKey []byte
	sendSeq    atomic.Uint64
	recvNext   uint64
	recvBuf    map[uint64][]byte
	recvMu     sync.Mutex
}

func newPortForwardConn(conn net.Conn, sendKey, receiveKey []byte) *portForwardConn {
	return &portForwardConn{
		conn:       conn,
		done:       make(chan struct{}),
		sendKey:    sendKey,
		receiveKey: receiveKey,
		recvBuf:    make(map[uint64][]byte),
	}
}

// close signals the reader goroutine to stop and closes the underlying TCP connection.
func (c *portForwardConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

func (c *portForwardConn) nextSendSeq() uint64 {
	return c.sendSeq.Add(1)
}

func (c *portForwardConn) deliver(seq uint64, plaintext []byte) error {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	c.recvBuf[seq] = plaintext
	if c.recvNext == 0 {
		c.recvNext = 1
	}

	for {
		chunk, ok := c.recvBuf[c.recvNext]
		if !ok {
			return nil
		}
		if _, err := c.conn.Write(chunk); err != nil {
			return err
		}
		delete(c.recvBuf, c.recvNext)
		c.recvNext++
	}
}
