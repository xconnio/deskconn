package deskconn

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sendPortEnvelope(conn net.Conn, kind byte, plaintext, key []byte) error {
	env, err := buildPortEnvelope(kind, plaintext, key)
	if err != nil {
		return err
	}
	return WriteFrame(conn, env)
}

func recvPortEnvelope(conn net.Conn, key []byte) (byte, []byte, error) {
	frame, err := ReadFrame(conn)
	if err != nil {
		return 0, nil, err
	}
	return DecryptEnvelope(frame, key)
}

// freeTCPPort picks a free port by briefly binding then releasing it.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	return port
}

func TestPortForwardConnectSuccessAndDataRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortForwardStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portForwardControlMsg{Port: portStr}), sendKey))
	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)
	var ack portForwardControlMsg
	require.NoError(t, json.Unmarshal(plaintext, &ack))
	require.Empty(t, ack.Error)

	require.NoError(t, sendPortEnvelope(client, portMsgData, []byte("hello"), sendKey))
	kind, plaintext, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgData, kind)
	require.Equal(t, []byte("hello"), plaintext)
}

func TestPortForwardConnectFailureReportsError(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortForwardStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	// Port 0 with no listener: dialing "localhost:0" fails immediately.
	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portForwardControlMsg{Port: "0"}), sendKey))
	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)
	var ack portForwardControlMsg
	require.NoError(t, json.Unmarshal(plaintext, &ack))
	require.NotEmpty(t, ack.Error, "a dial failure should report a descriptive error, not just close")
}

// TestPortForwardIgnoresPing confirms a no-op portMsgControl ping doesn't
// disrupt an otherwise-normal forwarded connection.
func TestPortForwardIgnoresPing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortForwardStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portForwardControlMsg{Port: portStr}), sendKey))
	_, _, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)

	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portForwardControlMsg{}), sendKey))
	require.NoError(t, sendPortEnvelope(client, portMsgData, []byte("still works"), sendKey))

	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgData, kind)
	require.Equal(t, []byte("still works"), plaintext)
}

// TestPortForwardClosesBackendOnClientDisconnect confirms the device's
// dialed backend connection closes soon after the client disconnects,
// without waiting for shellIdleTimeout -- net.Pipe's Close immediately
// errors the peer's blocked Read.
func TestPortForwardClosesBackendOnClientDisconnect(t *testing.T) {
	backendAcceptedCh := make(chan net.Conn, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		backendAcceptedCh <- conn
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	client, server := net.Pipe()
	d := Deskconn{}
	go d.handleQUICPortForwardStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portForwardControlMsg{Port: portStr}), sendKey))
	_, _, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)

	var backendConn net.Conn
	select {
	case backendConn = <-backendAcceptedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("device never dialed the backend")
	}
	t.Cleanup(func() { _ = backendConn.Close() })

	require.NoError(t, client.Close())

	require.Eventually(t, func() bool {
		one := make([]byte, 1)
		_ = backendConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, readErr := backendConn.Read(one)
		return readErr != nil
	}, 3*time.Second, 50*time.Millisecond, "backend connection should close once the client disconnects")
}

func TestPortReverseListenFailureReportsError(t *testing.T) {
	// Occupy a port so the device's listen call fails.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })
	_, portStr, err := net.SplitHostPort(occupied.Addr().String())
	require.NoError(t, err)

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortReverseStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portReverseMsg{Op: portRevOpListen, RemotePort: portStr}), sendKey))
	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)
	var ack portReverseMsg
	require.NoError(t, json.Unmarshal(plaintext, &ack))
	require.NotEmpty(t, ack.Error, "a listen failure should report a descriptive error")
}

func TestPortReverseSingleConnectionDataRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortReverseStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	portStr := freeTCPPort(t)
	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portReverseMsg{Op: portRevOpListen, RemotePort: portStr}), sendKey))
	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)
	var ack portReverseMsg
	require.NoError(t, json.Unmarshal(plaintext, &ack))
	require.Empty(t, ack.Error)

	extConn, err := net.Dial("tcp", "127.0.0.1:"+portStr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = extConn.Close() })

	kind, plaintext, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)
	var connMsg portReverseMsg
	require.NoError(t, json.Unmarshal(plaintext, &connMsg))
	require.Equal(t, portRevOpConnect, connMsg.Op)

	_, err = extConn.Write([]byte("hello"))
	require.NoError(t, err)

	kind, plaintext, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgData, kind)
	gotConnID, payload, ok := decodeConnData(plaintext)
	require.True(t, ok)
	require.Equal(t, connMsg.ConnID, gotConnID)
	require.Equal(t, []byte("hello"), payload)
}

// TestPortReverseConcurrentConnections drives several external connections
// through one port-reverse session at once and confirms every payload
// arrives intact and correctly attributed to its connID. This is the key
// correctness property that lets the new protocol drop the old WAMP-era
// seq/reorder-buffer scheme: it only holds if every send on the shared
// stream really does go through portReverseWriter's single owner.
func TestPortReverseConcurrentConnections(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := Deskconn{}
	go d.handleQUICPortReverseStream(server)

	sendKey, receiveKey, err := QuicClientKeyExchange(client)
	require.NoError(t, err)

	portStr := freeTCPPort(t)
	require.NoError(t, sendPortEnvelope(client, portMsgControl,
		mustJSON(portReverseMsg{Op: portRevOpListen, RemotePort: portStr}), sendKey))
	kind, _, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, portMsgControl, kind)

	const numConns = 8
	var wg sync.WaitGroup
	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, dialErr := net.Dial("tcp", "127.0.0.1:"+portStr)
			if dialErr != nil {
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(conn, "conn-%d-payload", i)
			time.Sleep(300 * time.Millisecond)
		}(i)
	}

	received := make(map[uint64]string)
	deadline := time.Now().Add(5 * time.Second)
	for len(received) < numConns && time.Now().Before(deadline) {
		_ = client.SetReadDeadline(deadline)
		kind, plaintext, recvErr := recvPortEnvelope(client, receiveKey)
		if recvErr != nil {
			break
		}
		if kind == portMsgData {
			connID, payload, ok := decodeConnData(plaintext)
			if ok {
				received[connID] = string(payload)
			}
		}
	}
	wg.Wait()

	require.Len(t, received, numConns, "every concurrent connection's payload should arrive intact")
	seenPayloads := make(map[string]bool, numConns)
	for _, p := range received {
		seenPayloads[p] = true
	}
	for i := 0; i < numConns; i++ {
		require.True(t, seenPayloads[fmt.Sprintf("conn-%d-payload", i)], "missing or corrupted payload for conn %d", i)
	}
}
