package deskconn

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	agentFwdMsgControl byte = iota // encrypted JSON agentForwardMsg
	agentFwdMsgData                // encrypted connID-tagged bytes, see encodeConnData
)

// agentFwdOp is agentForwardMsg's discriminator.
type agentFwdOp string

const (
	agentFwdOpStart   agentFwdOp = "start"   // client->device: begin forwarding, self-reporting AuthID
	agentFwdOpConnect agentFwdOp = "connect" // device->client: connID accepted
	agentFwdOpClose   agentFwdOp = "close"   // either direction: connID is done
)

// agentForwardMsg carries every agent-forward control message. Data itself
// travels as agentFwdMsgData envelopes tagged via encodeConnData, not
// through this struct. Op == "" is a ping.
type agentForwardMsg struct {
	Op     agentFwdOp `json:"op,omitempty"`
	ConnID uint64     `json:"conn_id,omitempty"`
	AuthID string     `json:"auth_id,omitempty"` // start only
	Error  string     `json:"error,omitempty"`   // start ack failure
}

// agentForwardSessions maps a client's self-reported AuthID to the private
// UNIX socket path currently forwarded for it, so shell.go's
// agentSockForAuthID can set SSH_AUTH_SOCK when starting a PTY. Each
// handleQUICAgentForwardStream/serveAgentForwardChannel session registers
// its own socket independently and unregisters it on its own disconnect -- if two
// sessions register for the same AuthID (e.g. two concurrent `shell -A` tabs),
// the latest registration wins the lookup.
type agentForwardSessions struct {
	paths map[string]string
	sync.Mutex
}

func newAgentForwardSessions() *agentForwardSessions {
	return &agentForwardSessions{paths: make(map[string]string)}
}

func (a *agentForwardSessions) register(authID, sockPath string) {
	a.Lock()
	a.paths[authID] = sockPath
	a.Unlock()
}

func (a *agentForwardSessions) unregister(authID, sockPath string) {
	a.Lock()
	if a.paths[authID] == sockPath {
		delete(a.paths, authID)
	}
	a.Unlock()
}

// socketPathByAuthID returns the forwarded agent socket path currently
// active for authID, if any.
func (a *agentForwardSessions) socketPathByAuthID(authID string) (string, bool) {
	a.Lock()
	defer a.Unlock()
	path, ok := a.paths[authID]
	return path, ok
}

// createAgentForwardSocket makes a private UNIX socket (0700 dir, 0600
// socket, matching ssh-agent's own default permissions) and listens on it.
func createAgentForwardSocket() (dir, sockPath string, ln net.Listener, err error) {
	dir, err = os.MkdirTemp("", "deskconn-agent-*")
	if err != nil {
		return "", "", nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil { // nolint: gosec
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}

	sockPath = filepath.Join(dir, "agent.sock")
	ln, err = net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	if err = os.Chmod(sockPath, 0600); err != nil { // nolint: gosec
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	return dir, sockPath, ln, nil
}

// handleQUICAgentForwardStream serves one agent-forward session over a raw
// QUIC stream: key exchange, read the client's start request, create the
// private socket, then multiplex every accepted connection's lifecycle and
// data over this one stream.
func (d *Deskconn) handleQUICAgentForwardStream(stream net.Conn) {
	defer stream.Close()

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
	frame, err := ReadFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
	if err != nil || kind != agentFwdMsgControl {
		return
	}
	var ctrl agentForwardMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != agentFwdOpStart {
		return
	}

	dir, sockPath, ln, createErr := createAgentForwardSocket()
	ack := agentForwardMsg{}
	if createErr != nil {
		ack.Error = createErr.Error()
	} else {
		defer os.RemoveAll(dir)
		defer ln.Close()
		d.agentForwardSessions.register(ctrl.AuthID, sockPath)
		defer d.agentForwardSessions.unregister(ctrl.AuthID, sockPath)
	}
	if env, buildErr := buildPortEnvelope(agentFwdMsgControl,
		mustJSON(ack), sendKey); buildErr == nil {
		_ = WriteFrame(stream, env)
	}
	if createErr != nil {
		return
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() {
		doneOnce.Do(func() {
			close(done)
			_ = ln.Close()
			_ = stream.Close()
		})
	}
	defer closeAll()

	writer := &portReverseWriter{ch: make(chan []byte, 64), done: done}
	SafeGo(func() {
		for {
			select {
			case env := <-writer.ch:
				if err := WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			case <-done:
				return
			}
		}
	})

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(agentFwdMsgControl,
					mustJSON(agentForwardMsg{}), sendKey)
				if err != nil {
					continue
				}
				writer.send(env)
			}
		}
	})

	var connCounter atomic.Uint64
	var mu sync.Mutex
	active := make(map[uint64]net.Conn)

	SafeGo(func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				closeAll()
				return
			}
			connID := connCounter.Add(1)
			mu.Lock()
			active[connID] = conn
			mu.Unlock()

			env, buildErr := buildPortEnvelope(agentFwdMsgControl,
				mustJSON(agentForwardMsg{Op: agentFwdOpConnect, ConnID: connID}), sendKey)
			if buildErr != nil || !writer.send(env) {
				conn.Close()
				mu.Lock()
				delete(active, connID)
				mu.Unlock()
				continue
			}

			SafeGo(func() {
				buf := make([]byte, 32*1024)
				for {
					n, readErr := conn.Read(buf)
					if n > 0 {
						dataEnv, encErr := buildPortEnvelope(agentFwdMsgData,
							encodeConnData(connID, buf[:n]), sendKey)
						if encErr != nil || !writer.send(dataEnv) {
							break
						}
					}
					if readErr != nil {
						break
					}
				}
				mu.Lock()
				_, stillActive := active[connID]
				delete(active, connID)
				mu.Unlock()
				conn.Close()
				if stillActive {
					closeEnv, encErr := buildPortEnvelope(agentFwdMsgControl,
						mustJSON(agentForwardMsg{Op: agentFwdOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.send(closeEnv)
					}
				}
			})
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := ReadFrame(stream)
		if err != nil {
			return
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case agentFwdMsgControl:
			var msg agentForwardMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != agentFwdOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case agentFwdMsgData:
			connID, payload, ok := decodeConnData(plaintext)
			if !ok {
				continue
			}
			mu.Lock()
			conn, ok := active[connID]
			mu.Unlock()
			if ok {
				_, _ = conn.Write(payload)
			}
		}
	}
}

// HandleAgentForwardChannel serves one agent-forward session over a raw
// WebRTC data channel, mirroring handleQUICAgentForwardStream.
func (d *Deskconn) HandleAgentForwardChannel(_ string, channel MessageChannel, firstMessage []byte) {
	SafeGo(func() { d.serveAgentForwardChannel(channel, firstMessage) })
}

func (d *Deskconn) serveAgentForwardChannel(channel MessageChannel, firstMessage []byte) {
	sendKey, receiveKey, err := P2PServerKeyExchange(channel, firstMessage)
	if err != nil {
		_ = channel.Close()
		return
	}

	closed, _ := WebrtcBackpressure(channel)
	msgCh := make(chan []byte, 32)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case msgCh <- msg.Data:
		case <-closed:
		}
	})

	first, err := RecvPriority(msgCh, closed, P2PRequestTimeout)
	if err != nil {
		_ = channel.Close()
		return
	}
	_, plaintext, err := DecryptEnvelope(first, receiveKey)
	if err != nil {
		_ = channel.Close()
		return
	}
	var ctrl agentForwardMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != agentFwdOpStart {
		_ = channel.Close()
		return
	}

	dir, sockPath, ln, createErr := createAgentForwardSocket()
	ack := agentForwardMsg{}
	if createErr != nil {
		ack.Error = createErr.Error()
	} else {
		defer os.RemoveAll(dir)
		defer ln.Close()
		d.agentForwardSessions.register(ctrl.AuthID, sockPath)
		defer d.agentForwardSessions.unregister(ctrl.AuthID, sockPath)
	}
	if env, buildErr := buildPortEnvelope(agentFwdMsgControl,
		mustJSON(ack), sendKey); buildErr == nil {
		_ = channel.Send(env)
	}
	if createErr != nil {
		_ = channel.Close()
		return
	}

	writer := &portReverseWriter{ch: make(chan []byte, 64), done: closed}
	SafeGo(func() {
		for {
			select {
			case env := <-writer.ch:
				if err := channel.Send(env); err != nil {
					_ = channel.Close()
					return
				}
			case <-closed:
				return
			}
		}
	})

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(agentFwdMsgControl,
					mustJSON(agentForwardMsg{}), sendKey)
				if err != nil {
					continue
				}
				writer.send(env)
			}
		}
	})

	var connCounter atomic.Uint64
	var mu sync.Mutex
	active := make(map[uint64]net.Conn)

	SafeGo(func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				_ = channel.Close()
				return
			}
			connID := connCounter.Add(1)
			mu.Lock()
			active[connID] = conn
			mu.Unlock()

			env, buildErr := buildPortEnvelope(agentFwdMsgControl,
				mustJSON(agentForwardMsg{Op: agentFwdOpConnect, ConnID: connID}), sendKey)
			if buildErr != nil || !writer.send(env) {
				conn.Close()
				mu.Lock()
				delete(active, connID)
				mu.Unlock()
				continue
			}

			SafeGo(func() {
				buf := make([]byte, 32*1024)
				for {
					n, readErr := conn.Read(buf)
					if n > 0 {
						dataEnv, encErr := buildPortEnvelope(agentFwdMsgData,
							encodeConnData(connID, buf[:n]), sendKey)
						if encErr != nil || !writer.send(dataEnv) {
							break
						}
					}
					if readErr != nil {
						break
					}
				}
				mu.Lock()
				_, stillActive := active[connID]
				delete(active, connID)
				mu.Unlock()
				conn.Close()
				if stillActive {
					closeEnv, encErr := buildPortEnvelope(agentFwdMsgControl,
						mustJSON(agentForwardMsg{Op: agentFwdOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.send(closeEnv)
					}
				}
			})
		}
	})

	for {
		frame, err := RecvPriority(msgCh, closed, shellIdleTimeout)
		if err != nil {
			return
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case agentFwdMsgControl:
			var msg agentForwardMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != agentFwdOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case agentFwdMsgData:
			connID, payload, ok := decodeConnData(plaintext)
			if !ok {
				continue
			}
			mu.Lock()
			conn, ok := active[connID]
			mu.Unlock()
			if ok {
				_, _ = conn.Write(payload)
			}
		}
	}
}
