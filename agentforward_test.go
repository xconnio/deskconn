package deskconn_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

func TestAgentForwardListenFailure(t *testing.T) {
	// os.TempDir() reads $TMPDIR on Unix but %TMP%/%TEMP% (not $TMPDIR) on Windows via
	// GetTempPath; set all three so the induced failure works on every platform.
	t.Setenv("TMPDIR", "/nonexistent-deskconn-test-dir-xyz")
	t.Setenv("TMP", "/nonexistent-deskconn-test-dir-xyz")
	t.Setenv("TEMP", "/nonexistent-deskconn-test-dir-xyz")
	_, caller := setupDeskconn(t)

	var receivedClose bool
	closedCh := make(chan struct{})
	sent := false

	callResp := caller.Call(deskconn.ProcedureAgentForward).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !sent {
				sent = true
				return xconn.NewProgress("C")
			}
			select {
			case <-closedCh:
			case <-ctx.Done():
			}
			return xconn.NewFinalProgress()
		}).
		ProgressReceiver(func(pr *xconn.ProgressResult) {
			if len(pr.Args()) < 2 {
				return
			}
			msgType, _ := pr.ArgString(0)
			if msgType != "X" {
				return
			}
			connID, err := pr.ArgUInt64(1)
			if err == nil && connID == 0 {
				receivedClose = true
				close(closedCh)
			}
		}).Do()

	require.NoError(t, callResp.Err)
	require.True(t, receivedClose)
}

func TestAgentForwardSocketPermissions(t *testing.T) {
	if runtime.GOOS == goosWindows {
		t.Skip("Windows does not support POSIX permission bits")
	}
	_, caller := setupDeskconn(t)

	readyCh := make(chan string, 1)
	sent := false

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	go func() {
		_ = caller.Call(deskconn.ProcedureAgentForward).
			ProgressSender(func(ctx context.Context) *xconn.Progress {
				if !sent {
					sent = true
					return xconn.NewProgress("C")
				}
				<-ctx.Done()
				return xconn.NewFinalProgress()
			}).
			ProgressReceiver(func(pr *xconn.ProgressResult) {
				if len(pr.Args()) < 2 {
					return
				}
				msgType, _ := pr.ArgString(0)
				if msgType != "R" {
					return
				}
				path, err := pr.ArgString(1)
				if err == nil {
					select {
					case readyCh <- path:
					default:
					}
				}
			}).DoContext(ctx)
	}()

	var sockPath string
	select {
	case sockPath = <-readyCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for msgReady")
	}

	info, err := os.Stat(sockPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Dir(sockPath))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), dirInfo.Mode().Perm())
}

func TestAgentForwardDataExchange(t *testing.T) {
	_, caller := setupDeskconn(t)

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
	var receivedData []byte
	var cbErr error
	firstSent := false
	keyExchangeSent := false

	callResp := caller.Call(deskconn.ProcedureAgentForward).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				return xconn.NewProgress("C")
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
					// Write from the "remote" connection, as if a process inside the
					// forwarded shell (e.g. ssh/git) spoke the agent protocol.
					remoteConn := <-remoteConnCh
					go func() { _, _ = remoteConn.Write([]byte("agent-request")) }()
					return xconn.NewProgress("K", ci.connID, clientPubKey)
				case <-ctx.Done():
					return xconn.NewFinalProgress()
				}
			}
			select {
			case data := <-echoCh:
				receivedData = data
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
			case "R":
				path, err := pr.ArgString(1)
				if err != nil {
					cbErr = err
					return
				}
				go func() {
					conn, dialErr := net.Dial("unix", path)
					if dialErr != nil {
						cbErr = dialErr
						return
					}
					remoteConnCh <- conn
					defer conn.Close()
					<-remoteDone
				}()
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
	require.Equal(t, []byte("agent-request"), receivedData)
}

func TestRunAgentForwardContextCancelled(t *testing.T) {
	_, caller := setupDeskconn(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ready := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.RunAgentForward(ctx, caller, "", "/nonexistent-agent.sock", ready)
	}()

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("RunAgentForward never signaled ready")
	}

	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("RunAgentForward did not return after context cancellation")
	}
}

func TestRunAgentForwardReadySignal(t *testing.T) {
	_, caller := setupDeskconn(t)

	dir := t.TempDir()
	agentSock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", agentSock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	ready := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.RunAgentForward(ctx, caller, "", agentSock, ready)
	}()

	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("RunAgentForward never signaled ready")
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("RunAgentForward did not return after context cancellation")
	}
}

func TestAgentForwardProxyReadySignal(t *testing.T) {
	deviceCallee, deviceCallerForProxy := setupRouterAndConnectSessions(t)
	d := deskconn.NewDeskconn(nil, nil, nil, false)
	require.NoError(t, d.Register(deviceCallee))

	localCallee, cliCaller := setupRouterAndConnectSessions(t)
	clientSessions := deskconn.NewClientSessions()
	deviceCtx, deviceCancel := context.WithCancel(context.Background())
	t.Cleanup(deviceCancel)
	const deviceRealm = "device-realm"
	clientSessions.StoreDeviceSession(deviceRealm, deviceCallerForProxy, nil, deviceCtx, deviceCancel)

	proxyCalls := deskconn.NewProxyCalls()
	regResp := localCallee.Register(deskconn.ProcedureProxyAgentForward,
		deskconn.ProxyProgressiveInvocationHandler(proxyCalls, clientSessions, t.TempDir(),
			deskconn.ProcedureAgentForward)).Do()
	require.NoError(t, regResp.Err)

	dir := t.TempDir()
	agentSock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", agentSock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()

	fwdCtx, fwdCancel := context.WithCancel(t.Context())
	t.Cleanup(fwdCancel)
	ready := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- deskconn.RunAgentForward(fwdCtx, cliCaller, deviceRealm, agentSock, ready)
	}()

	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("RunAgentForward never signaled ready over the proxy path")
	}

	fwdCancel()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("RunAgentForward did not return after context cancellation")
	}
}

func connectInMemoryWithAuthID(t *testing.T, r *xconn.Router, realm, authID string) *xconn.Session {
	t.Helper()
	base, err := xconn.ConnectInMemoryBase(r, realm, authID, "trusted", &serializers.MsgPackSerializer{}, 16)
	require.NoError(t, err)
	return xconn.NewSession(base, base.Serializer())
}

func TestAgentForwardMigrationReusesSocket(t *testing.T) {
	r, err := xconn.NewRouter(&xconn.RouterConfig{})
	require.NoError(t, err)
	require.NoError(t, r.AddRealm("realm1", xconn.DefaultRealmConfig()))

	callee, err := xconn.ConnectInMemory(r, "realm1")
	require.NoError(t, err)
	d := deskconn.NewDeskconn(nil, nil, nil, false)
	require.NoError(t, d.Register(callee))

	const sharedAuthID = "same-machine-identity"
	quicLike := connectInMemoryWithAuthID(t, r, "realm1", sharedAuthID)
	webrtcLike := connectInMemoryWithAuthID(t, r, "realm1", sharedAuthID)

	readyPath := func(ctx context.Context, session *xconn.Session, connCh chan<- uint64) <-chan string {
		ready := make(chan string, 1)
		sent := false
		go func() {
			_ = session.Call(deskconn.ProcedureAgentForward).
				ProgressSender(func(ctx context.Context) *xconn.Progress {
					if !sent {
						sent = true
						return xconn.NewProgress("C", uint64(0))
					}
					<-ctx.Done()
					return xconn.NewFinalProgress()
				}).
				ProgressReceiver(func(pr *xconn.ProgressResult) {
					if len(pr.Args()) < 2 {
						return
					}
					msgType, _ := pr.ArgString(0)
					switch msgType {
					case "R":
						if path, err := pr.ArgString(1); err == nil {
							select {
							case ready <- path:
							default:
							}
						}
					case "C":
						if connCh != nil {
							if connID, err := pr.ArgUInt64(1); err == nil {
								select {
								case connCh <- connID:
								default:
								}
							}
						}
					}
				}).DoContext(ctx)
		}()
		return ready
	}

	// First "connection" (simulating QUIC): creates the listener.
	firstCtx, firstCancel := context.WithCancel(context.Background())
	t.Cleanup(firstCancel)
	firstConnCh := make(chan uint64, 1)
	firstReady := readyPath(firstCtx, quicLike, firstConnCh)

	var firstSockPath string
	select {
	case firstSockPath = <-firstReady:
	case <-time.After(3 * time.Second):
		t.Fatal("first connect never became ready")
	}

	// Second "connection" (simulating the migrated WebRTC session, same identity, different
	// session ID): must report the *same* socket path, not create a new one.
	secondCtx, secondCancel := context.WithCancel(context.Background())
	t.Cleanup(secondCancel)
	secondConnCh := make(chan uint64, 1)
	secondReady := readyPath(secondCtx, webrtcLike, secondConnCh)

	var secondSockPath string
	select {
	case secondSockPath = <-secondReady:
	case <-time.After(3 * time.Second):
		t.Fatal("second (migrated) connect never became ready")
	}

	require.Equal(t, firstSockPath, secondSockPath,
		"migration should reuse the same forwarding socket, not create a new one")

	// A new connection to that (shared) socket should now be routed through the second,
	// current invocation — not the stale first one.
	conn, err := net.Dial("unix", secondSockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	select {
	case <-secondConnCh:
	case <-time.After(3 * time.Second):
		t.Fatal("post-migration connection was not routed through the current invocation")
	}
	select {
	case <-firstConnCh:
		t.Fatal("post-migration connection was incorrectly routed through the stale invocation")
	case <-time.After(200 * time.Millisecond):
	}
}
