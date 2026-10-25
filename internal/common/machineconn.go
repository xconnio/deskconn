package common

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"

	xconnd "github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn-core/bindings/go"
	"github.com/xconnio/xconn-core/bindings/go/auth"
)

// Standalone URL schemes: quic://host:port, or https://host:port[/path] for WebTransport.
const (
	SchemeQUIC         = "quic"
	SchemeWebTransport = "https"
)

// ParseStandaloneURL checks a standalone machine URL (see SchemeQUIC/SchemeWebTransport) and
// returns it, with WebTransport's path defaulting to xconnd.WebTransportPath.
func ParseStandaloneURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case SchemeQUIC, SchemeWebTransport:
	default:
		return nil, fmt.Errorf("unsupported url scheme %q (use quic://host:port, or https://host:port "+
			"for WebTransport)", u.Scheme)
	}
	if u.Host == "" || u.Port() == "" {
		return nil, fmt.Errorf("invalid url %q: missing host:port", rawURL)
	}
	if u.Scheme == SchemeWebTransport && (u.Path == "" || u.Path == "/") {
		u.Path = xconnd.WebTransportPath
	}
	return u, nil
}

// StandaloneTLSConfig is how the client verifies a standalone machine's certificate: pinned
// to certHash (its SHA-256 fingerprint, which userd prints) when set, e.g. for a
// self-signed certificate, otherwise against the system's trusted CAs.
func StandaloneTLSConfig(certHash string) (*tls.Config, error) {
	if certHash != "" {
		return PinnedTLSConfig(certHash)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13}, nil
}

// ConnectStandalone connects to the standalone machine at rawURL (see ParseStandaloneURL)
// over QUIC or WebTransport, and joins realm.
func ConnectStandalone(ctx context.Context, rawURL, certHash, realm string,
	authenticator auth.ClientAuthenticator) (*MachineConn, error) {
	u, err := ParseStandaloneURL(rawURL)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := StandaloneTLSConfig(certHash)
	if err != nil {
		return nil, err
	}

	if u.Scheme == SchemeQUIC {
		sess, err := xconn.ConnectQUIC(ctx, u.Host, realm,
			&xconn.QUICDialerConfig{Authenticator: authenticator, TLSConfig: tlsConfig})
		if err != nil {
			return nil, err
		}
		conn := NewQUICMachineConn(sess)
		conn.close = closeSessionAndConn(sess.Close, conn.closeConn)
		return conn, nil
	}

	sess, err := xconn.ConnectWebTransport(ctx, u.String(), realm,
		&xconn.WebTransportDialerConfig{Authenticator: authenticator, TLSClientConfig: tlsConfig})
	if err != nil {
		return nil, err
	}
	closeConn := func() error { return sess.Connection().CloseWithError(0, "") }
	return &MachineConn{
		Session:    sess.Session,
		openStream: sess.OpenStream,
		closeConn:  closeConn,
		close:      closeSessionAndConn(sess.Close, closeConn),
	}, nil
}

// closeSessionAndConn leaves the session, then closes the connection it was the only user of.
func closeSessionAndConn(closeSession, closeConn func() error) func() error {
	return func() error {
		err := closeSession()
		_ = closeConn()
		return err
	}
}

// MachineConn is a connection to a machine: one WAMP session plus raw streams opened
// alongside it, over QUIC (cloud or standalone) or WebTransport (standalone).
type MachineConn struct {
	*xconn.Session
	openStream func() (net.Conn, error)
	closeConn  func() error // the whole connection
	close      func() error // this session
}

// NewQUICMachineConn wraps a QUIC session.
func NewQUICMachineConn(s *xconn.QUICSession) *MachineConn {
	return &MachineConn{
		Session:    s.Session,
		openStream: s.OpenStream,
		closeConn:  s.Connection().Close,
		close:      s.Close,
	}
}

// OpenStream opens a raw stream to the machine.
func (c *MachineConn) OpenStream() (net.Conn, error) { return c.openStream() }

// Connection is the underlying connection shared by the session and its streams.
func (c *MachineConn) Connection() io.Closer { return closerFunc(c.closeConn) }

// Close leaves the WAMP session (and, for a standalone machine, closes the connection).
func (c *MachineConn) Close() error { return c.close() }

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
