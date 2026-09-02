package common_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
)

const yamuxTestRealm = "io.xconn.test"

// keyAuthenticator accepts cryptosign clients holding one public key.
type keyAuthenticator struct{ publicKey string }

func (a keyAuthenticator) Methods() []auth.Method { return []auth.Method{auth.MethodCryptoSign} }

func (a keyAuthenticator) Authenticate(request auth.Request) (auth.Response, error) {
	if r, ok := request.(*auth.RequestCryptoSign); ok && r.PublicKey() == a.publicKey {
		return auth.NewResponse(r.AuthID(), "owner", 0)
	}
	return nil, errors.New("unknown publickey")
}

func TestParseYamuxURL(t *testing.T) {
	network, address, err := common.ParseYamuxURL("tcp://0.0.0.0:18080")
	require.NoError(t, err)
	require.Equal(t, []string{"tcp", "0.0.0.0:18080"}, []string{network, address})

	network, address, err = common.ParseYamuxURL("unix:///tmp/deskconn.sock")
	require.NoError(t, err)
	require.Equal(t, []string{"unix", "/tmp/deskconn.sock"}, []string{network, address})

	for _, bad := range []string{"ws://host:1/ws", "tcp://", "unix://", "::"} {
		_, _, err := common.ParseYamuxURL(bad)
		require.Error(t, err, bad)
	}
}

func startYamuxServer(t *testing.T, rawURL, publicKey string) *common.YamuxListener {
	t.Helper()
	router, err := xconn.NewRouter(xconn.DefaultRouterConfig())
	require.NoError(t, err)
	require.NoError(t, router.AddRealm(yamuxTestRealm, &xconn.RealmConfig{Roles: []xconn.RealmRole{{
		Name:        "owner",
		Permissions: []xconn.Permission{{URI: "", MatchPolicy: "prefix", AllowCall: true}},
	}}}))
	t.Cleanup(router.Close)

	local, err := xconn.ConnectInMemory(router, yamuxTestRealm)
	require.NoError(t, err)
	resp := local.Register("io.xconn.test.echo", func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		return xconn.NewInvocationResult(inv.Args()...)
	}).Do()
	require.NoError(t, resp.Err)

	listener, err := common.ListenYamux(rawURL, router, keyAuthenticator{publicKey})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func connectYamux(listener *common.YamuxListener, privateKey string) (*common.DeviceConn, error) {
	authenticator, err := auth.NewCryptoSignAuthenticator("anyone", privateKey, nil)
	if err != nil {
		return nil, err
	}
	u := "tcp://" + listener.Addr().String()
	if listener.Addr().Network() == "unix" {
		u = common.UnixSocketURI(listener.Addr().String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return common.ConnectYamux(ctx, u, yamuxTestRealm, authenticator)
}

func requireRoundTrip(t *testing.T, listener *common.YamuxListener, conn *common.DeviceConn) {
	t.Helper()
	resp := conn.Call("io.xconn.test.echo").Args("hello").Do()
	require.NoError(t, resp.Err)
	require.Equal(t, "hello", resp.Args()[0])

	stream, err := conn.OpenStream()
	require.NoError(t, err)
	_, err = stream.Write([]byte("raw bytes"))
	require.NoError(t, err)

	select {
	case accepted := <-listener.Streams():
		require.Equal(t, conn.ID(), accepted.Session.ID())
		buf := make([]byte, len("raw bytes"))
		_, err := io.ReadFull(accepted, buf)
		require.NoError(t, err)
		require.Equal(t, "raw bytes", string(buf))
		_, err = accepted.Write([]byte("reply"))
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("raw stream was not delivered")
	}
	reply := make([]byte, len("reply"))
	_, err = io.ReadFull(stream, reply)
	require.NoError(t, err)
	require.Equal(t, "reply", string(reply))
}

func TestYamuxTCP(t *testing.T) {
	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	listener := startYamuxServer(t, "tcp://127.0.0.1:0", pub)

	conn, err := connectYamux(listener, priv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	requireRoundTrip(t, listener, conn)

	_, otherPriv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	_, err = connectYamux(listener, otherPriv)
	require.Error(t, err, "an unknown key must be refused")
}

func TestYamuxUnixSocket(t *testing.T) {
	pub, priv, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	listener := startYamuxServer(t, common.UnixSocketURI(filepath.Join(t.TempDir(), "d.sock")), pub)

	conn, err := connectYamux(listener, priv)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	requireRoundTrip(t, listener, conn)
}

// A connection that never authenticates gets its raw streams dropped.
func TestYamuxRawStreamRequiresAuth(t *testing.T) {
	pub, _, err := auth.GenerateCryptoSignKeyPair()
	require.NoError(t, err)
	listener := startYamuxServer(t, "tcp://127.0.0.1:0", pub)

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	mux, err := yamux.Client(conn, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mux.Close() })
	stream, err := mux.OpenStream()
	require.NoError(t, err)
	_, err = stream.Write([]byte(`{"op":"shell"}`))
	require.NoError(t, err)

	select {
	case <-listener.Streams():
		t.Fatal("unauthenticated raw stream was delivered")
	case <-time.After(6 * time.Second):
	}
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	_, err = stream.Read(make([]byte, 1))
	require.Error(t, err, "the dropped stream is closed")
}
