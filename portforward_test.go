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

func TestPortForwardNoProgress(t *testing.T) {
	_, caller := setupDeskconn(t)
	callResp := caller.Call(deskconn.ProcedurePortForward).Do()
	require.NoError(t, callResp.Err)
	require.Empty(t, callResp.Args())
}

// TestPortForwardConnectFailure verifies that when the server cannot establish
// a TCP connection, it sends a msgClose ("X") message back to the caller.
func TestPortForwardConnectFailure(t *testing.T) {
	_, caller := setupDeskconn(t)

	// Grab a free port then release it so the subsequent dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()

	connID := uint64(1)
	clientPubKey, _, err := deskconn.CreateX25519KeyPair()
	require.NoError(t, err)

	var receivedClose bool
	closedCh := make(chan struct{})
	sent := false

	callResp := caller.Call(deskconn.ProcedurePortForward).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("C", connID, "127.0.0.1", port, clientPubKey)
			}
			// Wait until the server has signalled "X" before closing.
			select {
			case <-closedCh:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress(connID)
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

// TestPortForwardConnectSuccess verifies that a successful TCP connection
// triggers a key exchange ("K") from the server and that the caller can
// cleanly close the forwarded connection afterwards.
func TestPortForwardConnectSuccess(t *testing.T) {
	_, caller := setupDeskconn(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		<-t.Context().Done()
		conn.Close()
	}()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	connID := uint64(1)

	clientPubKey, clientPrivKey, err := deskconn.CreateX25519KeyPair()
	require.NoError(t, err)

	var cbErr error
	keyReady := make(chan struct{})
	sent := false

	callResp := caller.Call(deskconn.ProcedurePortForward).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("C", connID, "127.0.0.1", port, clientPubKey)
			}
			// Close once the key exchange is done.
			select {
			case <-keyReady:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress(connID)
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 1 {
				return
			}
			msgType, _ := pr.ArgString(0)
			if msgType != "K" {
				return
			}
			serverPubKey, kErr := pr.ArgBytes(2)
			if kErr != nil {
				cbErr = kErr
				close(keyReady)
				return
			}
			sharedSecret, kErr := deskconn.PerformKeyExchange(clientPrivKey, serverPubKey)
			if kErr != nil {
				cbErr = kErr
				close(keyReady)
				return
			}
			_, kErr = deskconn.DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
			if kErr != nil {
				cbErr = kErr
			}
			close(keyReady)
		}).Do()

	require.NoError(t, cbErr)
	require.NoError(t, callResp.Err)
}

// TestPortForwardDataExchange verifies the full forwarding flow: connect, key
// exchange, send an encrypted payload, and receive the echoed reply.
func TestPortForwardDataExchange(t *testing.T) {
	_, caller := setupDeskconn(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
		<-t.Context().Done()
	}()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	connID := uint64(1)

	clientPubKey, clientPrivKey, err := deskconn.CreateX25519KeyPair()
	require.NoError(t, err)

	var sendKey, receiveKey []byte
	var cbErr error
	var receivedEcho []byte
	keyReady := make(chan struct{})
	echoCh := make(chan []byte, 1)
	connectSent := false
	keyExchanged := false
	dataSent := false

	callResp := caller.Call(deskconn.ProcedurePortForward).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !connectSent {
				connectSent = true
				return xconn.NewProgress("C", connID, "127.0.0.1", port, clientPubKey)
			}
			if !keyExchanged {
				select {
				case <-keyReady:
					keyExchanged = true
				case <-ctx.Done():
					return xconn.NewFinalProgress(connID)
				}
			}
			if !dataSent {
				dataSent = true
				encrypted, encErr := deskconn.EncryptPayload([]byte("hello"), sendKey)
				if encErr != nil {
					cbErr = encErr
					return xconn.NewFinalProgress(connID)
				}
				return xconn.NewProgress("D", connID, uint64(1), encrypted)
			}
			// Block until the echo arrives, then close.
			select {
			case echo := <-echoCh:
				receivedEcho = echo
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress(connID)
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 1 {
				return
			}
			msgType, _ := pr.ArgString(0)
			switch msgType {
			case "K":
				serverPubKey, kErr := pr.ArgBytes(2)
				if kErr != nil {
					cbErr = kErr
					close(keyReady)
					return
				}
				sharedSecret, kErr := deskconn.PerformKeyExchange(clientPrivKey, serverPubKey)
				if kErr != nil {
					cbErr = kErr
					close(keyReady)
					return
				}
				receiveKey, kErr = deskconn.DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
				if kErr != nil {
					cbErr = kErr
					close(keyReady)
					return
				}
				sendKey, kErr = deskconn.DeriveKeyHKDF(sharedSecret, []byte("frontendToBackend"))
				if kErr != nil {
					cbErr = kErr
				}
				close(keyReady)
			case "D":
				encrypted, _ := pr.ArgBytes(3)
				plaintext, _ := deskconn.DecryptPayload(encrypted, receiveKey)
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

// TestForwardLocalPortContextCancelled verifies that ForwardLocalPort returns
// promptly when its context is already cancelled.
func TestForwardLocalPortContextCancelled(t *testing.T) {
	_, caller := setupDeskconn(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.ForwardLocalPort(ctx, caller, "8080", "0")
	}()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("ForwardLocalPort did not return after context cancellation")
	}
}
