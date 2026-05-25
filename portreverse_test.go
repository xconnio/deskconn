package deskconn_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/xconn-go"
)

func TestPortReverseNoProgress(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedurePortReverse).Do()
	require.NoError(t, callResp.Err)
	require.Empty(t, callResp.Args())
}

// TestPortReverseListenFailure verifies that when the server cannot bind the
// requested port it sends a msgClose ("X") message back to the caller.
func TestPortReverseListenFailure(t *testing.T) {
	_, caller := setupDeskconn(t)

	// Occupy a port so the server cannot bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	var receivedClose bool
	closedCh := make(chan struct{})
	sent := false

	callResp := caller.Call(deskconn.ProcedurePortReverse).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("C", port)
			}
			select {
			case <-closedCh:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 1 {
				return
			}
			msgType, _ := pr.ArgString(0)
			if msgType == "X" {
				receivedClose = true
				close(closedCh)
			}
		}).Do()

	require.NoError(t, callResp.Err)
	require.True(t, receivedClose)
}

// TestPortReverseConnectSuccess verifies that when an external TCP client
// connects to the server-side port the server sends a msgConnect ("C") with a
// connID and its public key, and that the caller can complete the key exchange.
func TestPortReverseConnectSuccess(t *testing.T) {
	_, caller := setupDeskconn(t)

	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	remotePort := strconv.Itoa(tmp.Addr().(*net.TCPAddr).Port)
	tmp.Close()

	type connInfo struct {
		connID    uint64
		serverPub []byte
	}

	connCh := make(chan connInfo, 1)
	remoteDone := make(chan struct{})
	t.Cleanup(func() { close(remoteDone) })

	var cbErr error
	firstSent := false
	keyExchangeSent := false

	callResp := caller.Call(deskconn.ProcedurePortReverse).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				go func() {
					time.Sleep(50 * time.Millisecond)
					conn, dialErr := net.Dial("tcp", "127.0.0.1:"+remotePort)
					if dialErr != nil {
						return
					}
					defer conn.Close()
					<-remoteDone
				}()
				return xconn.NewProgress("C", remotePort)
			}
			if !keyExchangeSent {
				select {
				case ci := <-connCh:
					keyExchangeSent = true
					clientPubKey, _, kErr := deskconn.CreateX25519KeyPair()
					if kErr != nil {
						cbErr = kErr
						return xconn.NewFinalProgress()
					}
					return xconn.NewProgress("K", ci.connID, clientPubKey)
				case <-ctx.Done():
					return xconn.NewFinalProgress()
				}
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 2 {
				return
			}
			msgType, _ := pr.ArgString(0)
			if msgType != "C" {
				return
			}
			connID, kErr := pr.ArgUInt64(1)
			if kErr != nil {
				cbErr = kErr
				return
			}
			serverPub, kErr := pr.ArgBytes(2)
			if kErr != nil {
				cbErr = kErr
				return
			}
			connCh <- connInfo{connID, serverPub}
		}).Do()

	require.NoError(t, cbErr)
	require.NoError(t, callResp.Err)
}

// TestPortReverseDataExchange verifies the full reverse-port flow: an external
// TCP client connects, the key exchange completes, the client sends a payload,
// and the caller receives it correctly decrypted.
func TestPortReverseDataExchange(t *testing.T) {
	_, caller := setupDeskconn(t)

	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	remotePort := strconv.Itoa(tmp.Addr().(*net.TCPAddr).Port)
	tmp.Close()

	type connInfo struct {
		connID    uint64
		serverPub []byte
	}

	connCh := make(chan connInfo, 1)
	remoteConnCh := make(chan net.Conn, 1)
	remoteDone := make(chan struct{})
	t.Cleanup(func() { close(remoteDone) })
	echoCh := make(chan []byte, 1)

	var receiveKey []byte
	var receivedEcho []byte
	var cbErr error
	firstSent := false
	keyExchangeSent := false

	callResp := caller.Call(deskconn.ProcedurePortReverse).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				go func() {
					time.Sleep(50 * time.Millisecond)
					conn, dialErr := net.Dial("tcp", "127.0.0.1:"+remotePort)
					if dialErr != nil {
						return
					}
					remoteConnCh <- conn
					defer conn.Close()
					<-remoteDone
				}()
				return xconn.NewProgress("C", remotePort)
			}
			if !keyExchangeSent {
				select {
				case ci := <-connCh:
					keyExchangeSent = true
					clientPubKey, clientPrivKey, kErr := deskconn.CreateX25519KeyPair()
					if kErr != nil {
						cbErr = kErr
						return xconn.NewFinalProgress()
					}
					sharedSecret, kErr := deskconn.PerformKeyExchange(clientPrivKey, ci.serverPub)
					if kErr != nil {
						cbErr = kErr
						return xconn.NewFinalProgress()
					}
					receiveKey, kErr = deskconn.DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
					if kErr != nil {
						cbErr = kErr
						return xconn.NewFinalProgress()
					}
					// Write from the remote connection so the server reads and forwards.
					remoteConn := <-remoteConnCh
					go func() { _, _ = remoteConn.Write([]byte("hello")) }()
					return xconn.NewProgress("K", ci.connID, clientPubKey)
				case <-ctx.Done():
					return xconn.NewFinalProgress()
				}
			}
			select {
			case echo := <-echoCh:
				receivedEcho = echo
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 1 {
				return
			}
			msgType, _ := pr.ArgString(0)
			switch msgType {
			case "C":
				connID, kErr := pr.ArgUInt64(1)
				if kErr != nil {
					cbErr = kErr
					return
				}
				serverPub, kErr := pr.ArgBytes(2)
				if kErr != nil {
					cbErr = kErr
					return
				}
				connCh <- connInfo{connID, serverPub}
			case "D":
				if receiveKey == nil {
					return
				}
				encrypted, kErr := pr.ArgBytes(3)
				if kErr != nil {
					cbErr = kErr
					return
				}
				plaintext, kErr := deskconn.DecryptPayload(encrypted, receiveKey)
				if kErr != nil {
					cbErr = kErr
					return
				}
				select {
				case echoCh <- plaintext:
				default:
				}
			}
		}).Do()

	require.NoError(t, cbErr)
	require.NoError(t, callResp.Err)
	require.Equal(t, []byte("hello"), receivedEcho)
}

// TestReverseLocalPortContextCancelled verifies that ReverseLocalPort returns
// promptly when its context is already cancelled.
func TestReverseLocalPortContextCancelled(t *testing.T) {
	_, caller := setupDeskconn(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.ReverseLocalPort(ctx, caller, "0", "0")
	}()

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("ReverseLocalPort did not return after context cancellation")
	}
}

// TestReverseLocalPortDataExchange verifies the full high-level flow: the
// server listens on remotePort, an external client connects, data is forwarded
// to a local echo server and echoed back.
func TestReverseLocalPortDataExchange(t *testing.T) {
	_, caller := setupDeskconn(t)

	// Start a local echo server.
	localLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { localLn.Close() })
	localPort := strconv.Itoa(localLn.Addr().(*net.TCPAddr).Port)

	go func() {
		for {
			conn, acceptErr := localLn.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 256)
				n, _ := conn.Read(buf)
				_, _ = conn.Write(buf[:n])
			}()
		}
	}()

	// Pick a free remote port for the server to bind.
	tmp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	remotePort := strconv.Itoa(tmp.Addr().(*net.TCPAddr).Port)
	tmp.Close()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.ReverseLocalPort(ctx, caller, remotePort, localPort)
	}()

	var remoteConn net.Conn
	require.Eventually(t, func() bool {
		var dialErr error
		remoteConn, dialErr = net.Dial("tcp", "127.0.0.1:"+remotePort)
		return dialErr == nil
	}, 3*time.Second, 10*time.Millisecond)
	defer remoteConn.Close()

	_, err = remoteConn.Write([]byte("hello"))
	require.NoError(t, err)

	buf := make([]byte, 256)
	require.NoError(t, remoteConn.SetReadDeadline(time.Now().Add(3*time.Second)))
	n, err := remoteConn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), buf[:n])

	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ReverseLocalPort did not return after context cancellation")
	}
}
