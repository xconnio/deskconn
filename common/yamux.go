package common

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/transports"
	"github.com/xconnio/xconn-go"
)

// WAMP over yamux: one TCP (or Unix) connection carries a yamux session whose streams are
// either WAMP RawSocket sessions (first byte is the RawSocket magic) or raw streams -- the
// same model as xconn-go's QUIC transport, which xconn-go doesn't offer over yamux yet.

// yamuxRawStreamAuthTimeout bounds how long a raw stream waits for its connection to
// authenticate a WAMP session before it is dropped.
const yamuxRawStreamAuthTimeout = 5 * time.Second

// ParseYamuxURL returns the network and address of a tcp://host:port or unix:///path URL.
func ParseYamuxURL(rawURL string) (network, address string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "tcp":
		if u.Host == "" {
			return "", "", fmt.Errorf("invalid url %q: missing host:port", rawURL)
		}
		return "tcp", u.Host, nil
	case "unix":
		if u.Path == "" {
			return "", "", fmt.Errorf("invalid url %q: missing socket path", rawURL)
		}
		return "unix", u.Path, nil
	}
	return "", "", fmt.Errorf("unsupported url scheme %q (use tcp://host:port or unix:///path)", u.Scheme)
}

// YamuxStream is a raw stream delivered by YamuxListener, along with the authenticated
// WAMP session of the connection it arrived on.
type YamuxStream struct {
	net.Conn
	Session xconn.BaseSession
}

// YamuxListener serves WAMP over yamux: WAMP streams are attached to router after
// authentication; raw streams are delivered on Streams once their connection has an
// authenticated WAMP session.
type YamuxListener struct {
	ln       net.Listener
	router   *xconn.Router
	acceptor *xconn.RawSocketAcceptor
	streams  chan *YamuxStream
	done     chan struct{}
	once     sync.Once
}

// ListenYamux listens on rawURL (see ParseYamuxURL) for yamux clients.
func ListenYamux(rawURL string, router *xconn.Router, authenticator auth.ServerAuthenticator) (*YamuxListener,
	error) {
	network, address, err := ParseYamuxURL(rawURL)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}
	l := &YamuxListener{
		ln:       ln,
		router:   router,
		acceptor: &xconn.RawSocketAcceptor{Authenticator: authenticator},
		streams:  make(chan *YamuxStream),
		done:     make(chan struct{}),
	}
	SafeGo(l.acceptLoop)
	return l, nil
}

// Streams delivers authenticated connections' raw streams.
func (l *YamuxListener) Streams() <-chan *YamuxStream { return l.streams }

func (l *YamuxListener) Addr() net.Addr { return l.ln.Addr() }

func (l *YamuxListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.ln.Close()
}

func (l *YamuxListener) acceptLoop() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		SafeGo(func() { l.serveConn(conn) })
	}
}

func (l *YamuxListener) serveConn(conn net.Conn) {
	mux, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return
	}
	defer mux.Close()
	go func() {
		<-l.done
		_ = mux.Close()
	}()

	auth := newConnAuth()
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			return
		}
		SafeGo(func() { l.dispatch(mux, stream, auth) })
	}
}

// dispatch routes a stream: RawSocket magic first -> WAMP session, anything else -> raw stream.
func (l *YamuxListener) dispatch(mux *yamux.Session, stream *yamux.Stream, auth *connAuth) {
	br := bufio.NewReader(stream)
	magic, err := br.Peek(1)
	conn := &prependedConn{Reader: br, Conn: stream}
	if err == nil && magic[0] == transports.MAGIC {
		l.serveWAMP(conn, auth)
		return
	}

	session, ok := auth.wait(mux.CloseChan())
	if !ok {
		log.Debugf("yamux: dropping raw stream from %s: no authenticated session", mux.RemoteAddr())
		_ = stream.Close()
		return
	}
	select {
	case l.streams <- &YamuxStream{Conn: conn, Session: session}:
	case <-l.done:
		_ = stream.Close()
	}
}

func (l *YamuxListener) serveWAMP(conn net.Conn, auth *connAuth) {
	base, err := l.acceptor.Accept(conn, xconn.DefaultRawSocketServerConfig())
	if err != nil {
		log.Debugf("yamux: WAMP session rejected: %v", err)
		_ = conn.Close()
		return
	}
	if err := l.router.AttachClient(base); err != nil {
		log.Debugf("yamux: failed to attach session: %v", err)
		_ = conn.Close()
		return
	}
	auth.authenticated(base)

	for {
		msg, err := base.ReadMessage()
		if err != nil {
			_ = l.router.DetachClient(base)
			return
		}
		if err := l.router.ReceiveMessage(base, msg); err != nil {
			log.Debugf("yamux: %v", err)
		}
	}
}

// ConnectYamux dials a yamux server at rawURL and joins realm on its first stream.
func ConnectYamux(ctx context.Context, rawURL, realm string, authenticator auth.ClientAuthenticator) (*DeviceConn,
	error) {
	network, address, err := ParseYamuxURL(rawURL)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", rawURL, err)
	}
	mux, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	fail := func(err error) (*DeviceConn, error) {
		_ = mux.Close()
		return nil, err
	}

	stream, err := mux.OpenStream()
	if err != nil {
		return fail(err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	spec := xconn.CBORSerializerSpec
	serializerID := transports.Serializer(spec.SerializerID())
	header, err := transports.SendHandshake(transports.NewHandshake(serializerID, transports.DefaultMaxMsgSize))
	if err != nil {
		return fail(err)
	}
	if _, err := stream.Write(header); err != nil {
		return fail(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(stream, response); err != nil {
		return fail(fmt.Errorf("rawsocket handshake: %w", err))
	}
	if _, err := transports.ReceiveHandshake(response); err != nil {
		return fail(fmt.Errorf("rawsocket handshake: %w", err))
	}
	peer := xconn.NewRawSocketPeer(stream, xconn.RawSocketPeerConfig{
		Serializer: serializerID, OutQueueSize: xconn.ClientOutQueueSizeDefault})
	base, err := xconn.Join(peer, realm, spec.Serializer(), authenticator)
	if err != nil {
		return fail(err)
	}
	_ = stream.SetDeadline(time.Time{})

	session := xconn.NewSession(base, spec.Serializer())
	SafeGo(func() {
		<-session.Done()
		_ = mux.Close()
	})
	return &DeviceConn{
		Session:    session,
		openStream: func() (net.Conn, error) { return mux.OpenStream() },
		closeConn:  mux.Close,
		close: func() error {
			err := session.Leave()
			_ = mux.Close()
			return err
		},
	}, nil
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	return cfg
}

// prependedConn is a net.Conn whose reads come from Reader (which already buffered
// the bytes peeked off Conn).
type prependedConn struct {
	io.Reader
	net.Conn
}

func (c *prependedConn) Read(p []byte) (int, error) { return c.Reader.Read(p) }

// connAuth records the first authenticated WAMP session of a connection; raw streams
// on that connection wait for it.
type connAuth struct {
	once    sync.Once
	done    chan struct{}
	session xconn.BaseSession
}

func newConnAuth() *connAuth { return &connAuth{done: make(chan struct{})} }

func (a *connAuth) authenticated(session xconn.BaseSession) {
	a.once.Do(func() {
		a.session = session
		close(a.done)
	})
}

// wait returns the connection's authenticated session, allowing a client that opens a raw
// stream right after WELCOME to win the race; closed reports the connection going away.
func (a *connAuth) wait(closed <-chan struct{}) (xconn.BaseSession, bool) {
	select {
	case <-a.done:
		return a.session, true
	case <-closed:
	case <-time.After(yamuxRawStreamAuthTimeout):
	}
	return nil, false
}

// DeviceConn is a connection to a device: one WAMP session plus raw streams opened
// alongside it, over QUIC (cloud) or yamux (standalone).
type DeviceConn struct {
	*xconn.Session
	openStream func() (net.Conn, error)
	closeConn  func() error // the whole connection
	close      func() error // this session
}

// NewQUICDeviceConn wraps a QUIC session.
func NewQUICDeviceConn(s *xconn.QUICSession) *DeviceConn {
	return &DeviceConn{
		Session:    s.Session,
		openStream: s.OpenStream,
		closeConn:  s.Connection().Close,
		close:      s.Close,
	}
}

// OpenStream opens a raw stream to the device.
func (c *DeviceConn) OpenStream() (net.Conn, error) { return c.openStream() }

// Connection is the underlying connection shared by the session and its streams.
func (c *DeviceConn) Connection() io.Closer { return closerFunc(c.closeConn) }

// Close leaves the WAMP session (and, over yamux, closes the connection).
func (c *DeviceConn) Close() error { return c.close() }

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
