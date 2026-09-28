package xlink

import (
	"fmt"
	"net"
	"net/url"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

// ServeRouter serves the device realm on rawURL to clients holding one of keys: xlink's
// router on its own, with no cloud connection, config directory or deskconnd APIs. The
// scheme picks the transport: ws:// and rs:// listen on TCP, unix+ws:// and unix:// (or
// unix+rs://) on a Unix socket. stop shuts the router down.
func ServeRouter(rawURL, realm string, keys []string) (addr net.Addr, stop func(), err error) {
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("at least one key is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid url %q: %w", rawURL, err)
	}

	router := NewDeviceRouter(realm)
	authenticator := NewKeyAuthenticator(keys)
	server := xconn.NewServer(router, authenticator, &xconn.ServerConfig{})

	var listener *xconn.Listener
	switch u.Scheme {
	case "ws":
		listener, err = server.ListenAndServeWebSocket(xconn.NetworkTCP, u.Host)
	case "rs":
		listener, err = server.ListenAndServeRawSocket(xconn.NetworkTCP, u.Host)
	case "unix+ws":
		listener, err = server.ListenAndServeWebSocket(xconn.NetworkUnix, u.Path)
	case "unix", "unix+rs":
		listener, err = server.ListenAndServeRawSocket(xconn.NetworkUnix, u.Path)
	default:
		err = fmt.Errorf("unsupported url scheme %q (use ws, rs, unix, unix+ws or unix+rs)", u.Scheme)
	}
	if err != nil {
		router.Close()
		return nil, nil, err
	}
	stop = func() {
		_ = listener.Close()
		router.Close()
	}

	session, err := xconn.ConnectInMemory(router, realm)
	if err != nil {
		stop()
		return nil, nil, err
	}
	if err := SetupWebRTC(session, router, authenticator, ""); err != nil {
		stop()
		return nil, nil, err
	}
	return listener.Addr(), stop, nil
}

// keyAuthRole is the role every client of a keyAuthenticator gets.
const keyAuthRole = "owner"

// keyAuthenticator accepts cryptosign clients holding one of a fixed set of public keys,
// whatever authid they present.
type keyAuthenticator struct{ keys map[string]bool }

// NewKeyAuthenticator returns a keyAuthenticator for keys.
func NewKeyAuthenticator(keys []string) auth.ServerAuthenticator {
	a := &keyAuthenticator{keys: make(map[string]bool, len(keys))}
	for _, k := range keys {
		a.keys[k] = true
	}
	return a
}

func (a *keyAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (a *keyAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	r, ok := request.(*auth.RequestCryptoSign)
	if !ok || !a.keys[r.PublicKey()] {
		return nil, fmt.Errorf("unknown publickey")
	}
	return auth.NewResponse(r.AuthID(), keyAuthRole, 0)
}
