package deskconnd

import (
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/xconnio/deskconn/common"
)

// handleQUICPortForwardStream serves one forwarded TCP connection over a
// raw QUIC stream: key exchange, read the client's requested port, dial it
// locally, ack (or report the dial error), then relay.
func (d *Deskconn) handleQUICPortForwardStream(stream net.Conn) {
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
	if err != nil || kind != common.PortMsgControl {
		return
	}
	var ctrl common.PortForwardControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		return
	}

	tcpConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", ctrl.Port))
	ack := common.PortForwardControlMsg{}
	if dialErr != nil {
		ack.Error = dialErr.Error()
	}
	if env, buildErr := common.BuildPortEnvelope(common.PortMsgControl,
		common.MustJSON(ack), sendKey); buildErr == nil {
		_ = common.WriteFrame(stream, env)
	}
	if dialErr != nil {
		return
	}

	common.RelayPortForwardQUIC(stream, tcpConn, sendKey, receiveKey)
}

// HandlePortForwardChannel serves one forwarded TCP connection over a raw
// WebRTC data channel, mirroring handleQUICPortForwardStream.
func (d *Deskconn) HandlePortForwardChannel(_ string, channel common.MessageChannel, firstMessage []byte) {
	common.SafeGo(func() { d.servePortForwardChannel(channel, firstMessage) })
}

func (d *Deskconn) servePortForwardChannel(channel common.MessageChannel, firstMessage []byte) {
	sendKey, receiveKey, err := P2PServerKeyExchange(channel, firstMessage)
	if err != nil {
		_ = channel.Close()
		return
	}

	closed, _ := common.WebrtcBackpressure(channel)
	msgCh := make(chan []byte, 8)
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
	var ctrl common.PortForwardControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		_ = channel.Close()
		return
	}

	tcpConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", ctrl.Port))
	ack := common.PortForwardControlMsg{}
	if dialErr != nil {
		ack.Error = dialErr.Error()
	}
	_ = sendEncryptedJSONPort(channel, ack, sendKey)
	if dialErr != nil {
		_ = channel.Close()
		return
	}

	common.RelayPortForwardP2P(channel, tcpConn, sendKey, receiveKey, msgCh, closed)
}

// sendEncryptedJSONPort mirrors SendEncryptedJSON with port
// forward/reverse's own kind byte.
func sendEncryptedJSONPort(channel common.MessageChannel, v any, key []byte) error {
	envelope, err := common.BuildPortEnvelope(common.PortMsgControl, common.MustJSON(v), key)
	if err != nil {
		return err
	}
	return channel.Send(envelope)
}

// handleQUICPortReverseStream serves one port-reverse session over a raw
// QUIC stream: key exchange, read the client's listen request, start
// listening, then multiplex every accepted connection's lifecycle and data
// over this one stream (see PortReverseWriter).
func (d *Deskconn) handleQUICPortReverseStream(stream net.Conn) {
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
	if err != nil || kind != common.PortMsgControl {
		return
	}
	var ctrl common.PortReverseMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != common.PortRevOpListen {
		return
	}

	ln, listenErr := net.Listen("tcp", ":"+ctrl.RemotePort)
	ack := common.PortReverseMsg{Op: common.PortRevOpListen}
	if listenErr != nil {
		ack.Error = listenErr.Error()
	}
	if env, buildErr := common.BuildPortEnvelope(common.PortMsgControl,
		common.MustJSON(ack), sendKey); buildErr == nil {
		_ = common.WriteFrame(stream, env)
	}
	if listenErr != nil {
		return
	}
	defer ln.Close()

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
				env, err := common.BuildPortEnvelope(common.PortMsgControl,
					common.MustJSON(common.PortReverseMsg{}), sendKey)
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

			env, buildErr := common.BuildPortEnvelope(common.PortMsgControl,
				common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpConnect, ConnID: connID}), sendKey)
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
						dataEnv, encErr := common.BuildPortEnvelope(common.PortMsgData,
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
					closeEnv, encErr := common.BuildPortEnvelope(common.PortMsgControl,
						common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpClose, ConnID: connID}), sendKey)
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
		case common.PortMsgControl:
			var msg common.PortReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != common.PortRevOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case common.PortMsgData:
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

// HandlePortReverseChannel serves one port-reverse session over a raw
// WebRTC data channel, mirroring handleQUICPortReverseStream.
func (d *Deskconn) HandlePortReverseChannel(_ string, channel common.MessageChannel, firstMessage []byte) {
	common.SafeGo(func() { d.servePortReverseChannel(channel, firstMessage) })
}

func (d *Deskconn) servePortReverseChannel(channel common.MessageChannel, firstMessage []byte) {
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
	var ctrl common.PortReverseMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != common.PortRevOpListen {
		_ = channel.Close()
		return
	}

	ln, listenErr := net.Listen("tcp", ":"+ctrl.RemotePort)
	ack := common.PortReverseMsg{Op: common.PortRevOpListen}
	if listenErr != nil {
		ack.Error = listenErr.Error()
	}
	_ = sendEncryptedJSONPort(channel, ack, sendKey)
	if listenErr != nil {
		_ = channel.Close()
		return
	}
	defer ln.Close()

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
				env, err := common.BuildPortEnvelope(common.PortMsgControl,
					common.MustJSON(common.PortReverseMsg{}), sendKey)
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

			env, buildErr := common.BuildPortEnvelope(common.PortMsgControl,
				common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpConnect, ConnID: connID}), sendKey)
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
						dataEnv, encErr := common.BuildPortEnvelope(common.PortMsgData,
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
					closeEnv, encErr := common.BuildPortEnvelope(common.PortMsgControl,
						common.MustJSON(common.PortReverseMsg{Op: common.PortRevOpClose, ConnID: connID}), sendKey)
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
		case common.PortMsgControl:
			var msg common.PortReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != common.PortRevOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case common.PortMsgData:
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
