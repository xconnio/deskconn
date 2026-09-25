package deskconnd

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn/common"
)

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

	_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
	frame, err := common.ReadFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
	if err != nil || kind != common.AgentFwdMsgControl {
		return
	}
	var ctrl common.AgentForwardMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != common.AgentFwdOpStart {
		return
	}

	dir, sockPath, ln, createErr := createAgentForwardSocket()
	ack := common.AgentForwardMsg{}
	if createErr != nil {
		ack.Error = createErr.Error()
	} else {
		defer os.RemoveAll(dir)
		defer ln.Close()
		d.agentForwardSessions.register(ctrl.AuthID, sockPath)
		defer d.agentForwardSessions.unregister(ctrl.AuthID, sockPath)
	}
	if env, buildErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
		common.MustJSON(ack), sendKey); buildErr == nil {
		_ = common.WriteFrame(stream, env)
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

	writer := &common.PortReverseWriter{Ch: make(chan []byte, 64), Done: done}
	common.SafeGo(func() {
		for {
			select {
			case env := <-writer.Ch:
				if err := common.WriteFrame(stream, env); err != nil {
					closeAll()
					return
				}
			case <-done:
				return
			}
		}
	})

	common.SafeGo(func() {
		ticker := time.NewTicker(common.ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				env, err := common.BuildPortEnvelope(common.AgentFwdMsgControl,
					common.MustJSON(common.AgentForwardMsg{}), sendKey)
				if err != nil {
					continue
				}
				writer.Send(env)
			}
		}
	})

	var connCounter atomic.Uint64
	var mu sync.Mutex
	active := make(map[uint64]net.Conn)

	common.SafeGo(func() {
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

			env, buildErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
				common.MustJSON(common.AgentForwardMsg{Op: common.AgentFwdOpConnect, ConnID: connID}), sendKey)
			if buildErr != nil || !writer.Send(env) {
				conn.Close()
				mu.Lock()
				delete(active, connID)
				mu.Unlock()
				continue
			}

			common.SafeGo(func() {
				buf := make([]byte, 32*1024)
				for {
					n, readErr := conn.Read(buf)
					if n > 0 {
						dataEnv, encErr := common.BuildPortEnvelope(common.AgentFwdMsgData,
							common.EncodeConnData(connID, buf[:n]), sendKey)
						if encErr != nil || !writer.Send(dataEnv) {
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
					closeEnv, encErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
						common.MustJSON(common.AgentForwardMsg{Op: common.AgentFwdOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.Send(closeEnv)
					}
				}
			})
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
		frame, err := common.ReadFrame(stream)
		if err != nil {
			return
		}
		kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case common.AgentFwdMsgControl:
			var msg common.AgentForwardMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != common.AgentFwdOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case common.AgentFwdMsgData:
			connID, payload, ok := common.DecodeConnData(plaintext)
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
func (d *Deskconn) HandleAgentForwardChannel(_ string, channel common.MessageChannel, firstMessage []byte) {
	common.SafeGo(func() { d.serveAgentForwardChannel(channel, firstMessage) })
}

func (d *Deskconn) serveAgentForwardChannel(channel common.MessageChannel, firstMessage []byte) {
	sendKey, receiveKey, err := P2PServerKeyExchange(channel, firstMessage)
	if err != nil {
		_ = channel.Close()
		return
	}

	closed, _ := common.WebrtcBackpressure(channel)
	msgCh := make(chan []byte, 32)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case msgCh <- msg.Data:
		case <-closed:
		}
	})

	first, err := common.RecvPriority(msgCh, closed, common.P2PRequestTimeout)
	if err != nil {
		_ = channel.Close()
		return
	}
	_, plaintext, err := common.DecryptEnvelope(first, receiveKey)
	if err != nil {
		_ = channel.Close()
		return
	}
	var ctrl common.AgentForwardMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != common.AgentFwdOpStart {
		_ = channel.Close()
		return
	}

	dir, sockPath, ln, createErr := createAgentForwardSocket()
	ack := common.AgentForwardMsg{}
	if createErr != nil {
		ack.Error = createErr.Error()
	} else {
		defer os.RemoveAll(dir)
		defer ln.Close()
		d.agentForwardSessions.register(ctrl.AuthID, sockPath)
		defer d.agentForwardSessions.unregister(ctrl.AuthID, sockPath)
	}
	if env, buildErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
		common.MustJSON(ack), sendKey); buildErr == nil {
		_ = channel.Send(env)
	}
	if createErr != nil {
		_ = channel.Close()
		return
	}

	writer := &common.PortReverseWriter{Ch: make(chan []byte, 64), Done: closed}
	common.SafeGo(func() {
		for {
			select {
			case env := <-writer.Ch:
				if err := channel.Send(env); err != nil {
					_ = channel.Close()
					return
				}
			case <-closed:
				return
			}
		}
	})

	common.SafeGo(func() {
		ticker := time.NewTicker(common.ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
				env, err := common.BuildPortEnvelope(common.AgentFwdMsgControl,
					common.MustJSON(common.AgentForwardMsg{}), sendKey)
				if err != nil {
					continue
				}
				writer.Send(env)
			}
		}
	})

	var connCounter atomic.Uint64
	var mu sync.Mutex
	active := make(map[uint64]net.Conn)

	common.SafeGo(func() {
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

			env, buildErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
				common.MustJSON(common.AgentForwardMsg{Op: common.AgentFwdOpConnect, ConnID: connID}), sendKey)
			if buildErr != nil || !writer.Send(env) {
				conn.Close()
				mu.Lock()
				delete(active, connID)
				mu.Unlock()
				continue
			}

			common.SafeGo(func() {
				buf := make([]byte, 32*1024)
				for {
					n, readErr := conn.Read(buf)
					if n > 0 {
						dataEnv, encErr := common.BuildPortEnvelope(common.AgentFwdMsgData,
							common.EncodeConnData(connID, buf[:n]), sendKey)
						if encErr != nil || !writer.Send(dataEnv) {
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
					closeEnv, encErr := common.BuildPortEnvelope(common.AgentFwdMsgControl,
						common.MustJSON(common.AgentForwardMsg{Op: common.AgentFwdOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.Send(closeEnv)
					}
				}
			})
		}
	})

	for {
		frame, err := common.RecvPriority(msgCh, closed, common.ShellIdleTimeout)
		if err != nil {
			return
		}
		kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case common.AgentFwdMsgControl:
			var msg common.AgentForwardMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != common.AgentFwdOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case common.AgentFwdMsgData:
			connID, payload, ok := common.DecodeConnData(plaintext)
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
