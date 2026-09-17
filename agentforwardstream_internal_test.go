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

const testAuthID = "alice"

func newTestAgentForwardDeskconn() *Deskconn {
	return &Deskconn{agentForwardSessions: newAgentForwardSessions()}
}

func TestAgentForwardStartSuccessAndDataRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := newTestAgentForwardDeskconn()
	go d.handleQUICAgentForwardStream(server)

	sendKey, receiveKey, err := quicClientKeyExchange(client)
	require.NoError(t, err)

	require.NoError(t, sendPortEnvelope(client, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: testAuthID}), sendKey))
	kind, plaintext, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, agentFwdMsgControl, kind)
	var ack agentForwardMsg
	require.NoError(t, json.Unmarshal(plaintext, &ack))
	require.Empty(t, ack.Error)

	sockPath, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
	require.True(t, ok)
	require.NotEmpty(t, sockPath)

	extConn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = extConn.Close() })

	kind, plaintext, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, agentFwdMsgControl, kind)
	var connMsg agentForwardMsg
	require.NoError(t, json.Unmarshal(plaintext, &connMsg))
	require.Equal(t, agentFwdOpConnect, connMsg.Op)

	_, err = extConn.Write([]byte("hello"))
	require.NoError(t, err)

	kind, plaintext, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, agentFwdMsgData, kind)
	gotConnID, payload, ok := decodeConnData(plaintext)
	require.True(t, ok)
	require.Equal(t, connMsg.ConnID, gotConnID)
	require.Equal(t, []byte("hello"), payload)
}

func TestAgentForwardStartFailureReportsError(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := newTestAgentForwardDeskconn()
	go d.handleQUICAgentForwardStream(server)

	sendKey, _, err := quicClientKeyExchange(client)
	require.NoError(t, err)

	// A control message that isn't a "start" op is rejected instead of acked.
	require.NoError(t, sendPortEnvelope(client, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpClose, AuthID: testAuthID}), sendKey))

	one := make([]byte, 1)
	_ = client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, readErr := client.Read(one)
	require.Error(t, readErr, "an invalid start request should close the stream, not ack it")
}

func TestAgentForwardSocketPathCompareAndDeleteOnCleanup(t *testing.T) {
	client1, server1 := net.Pipe()
	d := newTestAgentForwardDeskconn()
	go d.handleQUICAgentForwardStream(server1)

	sendKey1, receiveKey1, err := quicClientKeyExchange(client1)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client1, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: testAuthID}), sendKey1))
	_, _, err = recvPortEnvelope(client1, receiveKey1)
	require.NoError(t, err)

	sockPath1, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
	require.True(t, ok)

	client2, server2 := net.Pipe()
	t.Cleanup(func() { _ = client2.Close() })
	go d.handleQUICAgentForwardStream(server2)

	sendKey2, receiveKey2, err := quicClientKeyExchange(client2)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client2, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: testAuthID}), sendKey2))
	_, _, err = recvPortEnvelope(client2, receiveKey2)
	require.NoError(t, err)

	sockPath2, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
	require.True(t, ok)
	require.NotEqual(t, sockPath1, sockPath2, "two independent sessions get independent sockets")

	// Closing the OLDER session must not clobber the newer session's live registration.
	require.NoError(t, client1.Close())
	require.Eventually(t, func() bool {
		p, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
		return ok && p == sockPath2
	}, 3*time.Second, 20*time.Millisecond, "newer session's registration should survive the older session's cleanup")

	// Closing the newer (currently registered) session removes the entry entirely.
	require.NoError(t, client2.Close())
	require.Eventually(t, func() bool {
		_, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
		return !ok
	}, 3*time.Second, 20*time.Millisecond, "the currently-registered session's cleanup should remove the entry")
}

func TestAgentForwardIgnoresPing(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := newTestAgentForwardDeskconn()
	go d.handleQUICAgentForwardStream(server)

	sendKey, receiveKey, err := quicClientKeyExchange(client)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: testAuthID}), sendKey))
	_, _, err = recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)

	require.NoError(t, sendPortEnvelope(client, agentFwdMsgControl, mustJSON(agentForwardMsg{}), sendKey))

	sockPath, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
	require.True(t, ok)
	extConn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = extConn.Close() })

	kind, _, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, agentFwdMsgControl, kind, "the connect notice should still arrive after the ping")
}

// TestAgentForwardConcurrentConnections drives several external connections
// through one agent-forward session at once and confirms every payload
// arrives intact and correctly attributed to its connID, relying on the
// same single-writer ordering guarantee port reverse's equivalent test
// verifies (see portReverseWriter's doc comment).
func TestAgentForwardConcurrentConnections(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	d := newTestAgentForwardDeskconn()
	go d.handleQUICAgentForwardStream(server)

	sendKey, receiveKey, err := quicClientKeyExchange(client)
	require.NoError(t, err)
	require.NoError(t, sendPortEnvelope(client, agentFwdMsgControl,
		mustJSON(agentForwardMsg{Op: agentFwdOpStart, AuthID: testAuthID}), sendKey))
	kind, _, err := recvPortEnvelope(client, receiveKey)
	require.NoError(t, err)
	require.Equal(t, agentFwdMsgControl, kind)

	sockPath, ok := d.agentForwardSessions.socketPathByAuthID(testAuthID)
	require.True(t, ok)

	const numConns = 8
	var wg sync.WaitGroup
	for i := range numConns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, dialErr := net.Dial("unix", sockPath)
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
		if kind == agentFwdMsgData {
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
	for i := range numConns {
		require.True(t, seenPayloads[fmt.Sprintf("conn-%d-payload", i)], "missing or corrupted payload for conn %d", i)
	}
}
