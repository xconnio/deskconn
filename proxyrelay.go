package deskconn

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

// ProxyRelaySocketName is the Unix socket file name (inside the config
// directory) the local daemon listens on for raw file-transfer relay
// connections -- the proxy-mode ("cp" with no --mode flag) equivalent of
// --mode quic's direct connection. Every worker's stream rides the
// daemon's own persistent, cached connection to the target device instead
// of each worker authenticating to the cloud router fresh.
const ProxyRelaySocketName = "filetransfer.sock"

// ProxyRelaySocketPath returns the full path to the proxy relay socket.
func ProxyRelaySocketPath(cfgDirectory string) string {
	return filepath.Join(cfgDirectory, ProxyRelaySocketName)
}

// relayHello is the first message a client sends on a relay connection,
// naming which device realm to relay this connection to. Everything after
// it is a transparent, unparsed byte pipe to a stream opened on the
// daemon's persistent QUIC connection to that realm -- the same
// fsRequest/fsResponse wire protocol quictransfer.go's client and server
// already speak to each other directly in --mode quic, so the daemon never
// needs to parse or understand it, only relay it.
type relayHello struct {
	Realm string `json:"realm"`
}

// proxyRelayConn adapts one dialed relay connection to
// xconn.MultiplexedSession: its single OpenStream call returns that same
// connection, since from the client's perspective one relay connection is
// one stream -- the daemon on the other end is what multiplexes many such
// relay connections onto its single persistent connection to the device.
type proxyRelayConn struct {
	conn net.Conn
}

func (p *proxyRelayConn) OpenStream() (net.Conn, error) {
	return p.conn, nil
}

func (p *proxyRelayConn) Close() error {
	return p.conn.Close()
}

// DialProxyRelay opens one new relay connection to the local daemon's
// proxy relay socket, targeting realm, and returns it ready to hand to
// DownloadFilesQUIC/UploadFilesQUIC as either the control session or as what a
// QUICConnector returns for one worker.
func DialProxyRelay(socketPath, realm string) (*proxyRelayConn, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to local deskconnd (is it running?): %w", err)
	}
	if err := writeMsg(conn, relayHello{Realm: realm}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &proxyRelayConn{conn: conn}, nil
}

// ProxyQUICConnector returns a QUICConnector that opens a fresh relay
// connection to realm through the local daemon on demand -- one per
// parallel worker, each multiplexed by the daemon onto its single
// persistent connection to the device.
func ProxyQUICConnector(socketPath, realm string) QUICConnector {
	return func() (QUICStreamCloser, error) {
		return DialProxyRelay(socketPath, realm)
	}
}

// QUICSessionCache maintains one persistent *xconn.QUICSession per device
// realm, reused across every proxy relay request instead of dialing fresh
// each time -- this is what lets proxy-mode transfers reuse a warm
// connection across separate "cp" invocations for as long as this daemon
// process keeps running.
type QUICSessionCache struct {
	mu       sync.Mutex
	sessions map[string]*xconn.QUICSession
}

func NewQUICSessionCache() *QUICSessionCache {
	return &QUICSessionCache{sessions: make(map[string]*xconn.QUICSession)}
}

func (c *QUICSessionCache) getOrConnect(ctx context.Context, realm, cfgDirectory string) (*xconn.QUICSession, error) {
	c.mu.Lock()
	if sess, ok := c.sessions[realm]; ok && sess.Connected() {
		c.mu.Unlock()
		return sess, nil
	}
	c.mu.Unlock()

	sess, err := ConnectDeviceRealmQUIC(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.sessions[realm] = sess
	c.mu.Unlock()
	return sess, nil
}

func (c *QUICSessionCache) invalidate(realm string) {
	c.mu.Lock()
	delete(c.sessions, realm)
	c.mu.Unlock()
}

// ServeProxyRelay accepts connections on listener and relays each one,
// transparently, to a stream opened on the persistent QUIC connection
// cached for the realm its relayHello names. Blocks until listener errors
// (typically because it was closed).
func ServeProxyRelay(ctx context.Context, listener net.Listener, cache *QUICSessionCache, cfgDirectory string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		SafeGo(func() { handleProxyRelayConn(ctx, conn, cache, cfgDirectory) })
	}
}

func handleProxyRelayConn(ctx context.Context, conn net.Conn, cache *QUICSessionCache, cfgDirectory string) {
	defer conn.Close()

	var hello relayHello
	if err := readMsg(conn, &hello); err != nil {
		log.Debugf("proxyrelay: reading hello: %v", err)
		return
	}
	if hello.Realm == "" {
		return
	}

	deviceStream, err := openDeviceStream(ctx, cache, hello.Realm, cfgDirectory)
	if err != nil {
		log.Debugf("proxyrelay: opening device stream for realm=%q: %v", hello.Realm, err)
		// Reported as an fsResponse so the client's very first readMsg (the
		// one it does right after sending its list/init/read/write request)
		// surfaces a real diagnostic instead of a bare "connection closed".
		_ = writeMsg(conn, fsResponse{Err: fmt.Sprintf("connecting to device: %v", err)})
		return
	}
	defer deviceStream.Close()

	relayBytes(conn, deviceStream)
}

// openDeviceStream opens a stream on the cached persistent connection for
// realm, reconnecting once if the cached connection turns out to be dead
// (e.g. the device restarted since it was cached).
func openDeviceStream(ctx context.Context, cache *QUICSessionCache, realm, cfgDirectory string) (net.Conn, error) {
	quicSess, err := cache.getOrConnect(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}

	stream, err := quicSess.OpenStream()
	if err == nil {
		return stream, nil
	}

	cache.invalidate(realm)
	quicSess, err = cache.getOrConnect(ctx, realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	return quicSess.OpenStream()
}

// relayBytes transparently pipes bytes both ways between a and b until
// both directions finish (each side reaches EOF or errors). The
// fsRequest/fsResponse protocol carried over this pipe is synchronous
// request-then-full-response, so by the time either side closes cleanly
// the other has nothing left to say -- no half-close handling needed.
func relayBytes(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	SafeGo(func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
	})
	SafeGo(func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
	})
	wg.Wait()
}
