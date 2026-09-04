package deskconn

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRelayBytesBidirectional(t *testing.T) {
	aClient, aServer := net.Pipe()
	bClient, bServer := net.Pipe()
	defer aClient.Close()
	defer bClient.Close()

	done := make(chan struct{})
	go func() {
		relayBytes(aServer, bServer)
		close(done)
	}()

	// a -> relay -> b
	go func() { _, _ = aClient.Write([]byte("hello from a")) }()
	buf := make([]byte, 32)
	n, err := bClient.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello from a", string(buf[:n]))

	// b -> relay -> a
	go func() { _, _ = bClient.Write([]byte("hello from b")) }()
	n, err = aClient.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello from b", string(buf[:n]))

	_ = aClient.Close()
	_ = bClient.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relayBytes never returned after both sides closed")
	}
}

func TestDialProxyRelaySendsHello(t *testing.T) {
	dir := t.TempDir()
	socketPath := ProxyRelaySocketPath(dir)

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	helloCh := make(chan relayHello, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		var hello relayHello
		if readMsg(conn, &hello) == nil {
			helloCh <- hello
		}
	}()

	relayConn, err := DialProxyRelay(socketPath, "realm.under.test")
	require.NoError(t, err)
	defer relayConn.Close()

	select {
	case hello := <-helloCh:
		require.Equal(t, "realm.under.test", hello.Realm)
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the relay hello")
	}
}

func TestDialProxyRelayNoDaemonRunning(t *testing.T) {
	dir := t.TempDir()
	_, err := DialProxyRelay(ProxyRelaySocketPath(dir), "some-realm")
	require.Error(t, err)
}

func TestProxyRelayConnOpenStreamReturnsSameConn(t *testing.T) {
	dir := t.TempDir()
	socketPath := ProxyRelaySocketPath(dir)

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			var hello relayHello
			_ = readMsg(conn, &hello)
		}
	}()

	relayConn, err := DialProxyRelay(socketPath, "realm")
	require.NoError(t, err)
	defer relayConn.Close()

	s1, err := relayConn.OpenStream()
	require.NoError(t, err)
	s2, err := relayConn.OpenStream()
	require.NoError(t, err)
	require.Same(t, s1, s2, "OpenStream should always return the same underlying connection")
}
