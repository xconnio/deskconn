package deskconn

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// portForwardChannelLabel/portReverseChannelLabel are checked by
// HandleAuxDataChannel (iptunnel.go) before file transfer's first-message
// sniffing, same as shellChannelLabel.
const (
	portForwardChannelLabel = "portforward"
	portReverseChannelLabel = "portreverse"
)

// Envelope kind bytes, mirroring shellMsgControl/shellMsgData but in their
// own namespace since these features never share a connection with
// shell/file-transfer.
const (
	portMsgControl byte = iota // encrypted JSON control message
	portMsgData                // encrypted relayed bytes (port reverse: connID-tagged, see encodeConnData)
)

// buildPortEnvelope encrypts plaintext and prepends kind, ready to hand to
// either transport's raw send.
func buildPortEnvelope(kind byte, plaintext, key []byte) ([]byte, error) {
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 1+len(ciphertext))
	envelope[0] = kind
	copy(envelope[1:], ciphertext)
	return envelope, nil
}

// portForwardControlMsg is port forward's only control message: the
// client's "dial this port" request, the device's ack/Error, or -- both
// fields empty -- a ping keeping shellIdleTimeout from firing on a quiet
// connection.
type portForwardControlMsg struct {
	Port  string `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

// encodeConnData/decodeConnData tag a port-reverse data chunk with which
// forwarded connection it belongs to: 8-byte big-endian connID, then the
// raw payload. Binary rather than JSON so per-chunk overhead stays minimal
// on what can be high-volume traffic.
func encodeConnData(connID uint64, payload []byte) []byte {
	buf := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint64(buf, connID)
	copy(buf[8:], payload)
	return buf
}

func decodeConnData(data []byte) (connID uint64, payload []byte, ok bool) {
	if len(data) < 8 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64(data), data[8:], true
}

// portReverseOp is portReverseMsg's discriminator.
type portReverseOp string

const (
	portRevOpListen  portReverseOp = "listen"  // client->device: start listening on RemotePort
	portRevOpConnect portReverseOp = "connect" // device->client: connID accepted, client should dial its local port
	portRevOpClose   portReverseOp = "close"   // either direction: connID is done
)

// portReverseMsg carries every port-reverse control message: connection
// lifecycle events multiplexed over one session stream. Data itself travels
// as portMsgData envelopes tagged via encodeConnData, not through this
// struct. Op == "" is a ping.
type portReverseMsg struct {
	Op         portReverseOp `json:"op,omitempty"`
	ConnID     uint64        `json:"conn_id,omitempty"`
	RemotePort string        `json:"remote_port,omitempty"` // listen only
	Error      string        `json:"error,omitempty"`       // listen ack failure
}

// relayPortForwardQUIC bidirectionally relays tcpConn's bytes over stream,
// encrypted, until either side closes or goes idle past shellIdleTimeout.
// Used by both the device (relaying to the dialed backend) and the client
// (relaying to the locally accepted connection) -- the relay itself doesn't
// care which side opened the stream, so one implementation serves both.
func relayPortForwardQUIC(stream net.Conn, tcpConn net.Conn, sendKey, receiveKey []byte) {
	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			close(done)
			_ = tcpConn.Close()
			_ = stream.Close()
		})
	}
	defer closeAll()

	SafeGo(func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				envelope, encErr := buildPortEnvelope(portMsgData, buf[:n], sendKey)
				if encErr != nil {
					closeAll()
					return
				}
				if wErr := writeFrame(stream, envelope); wErr != nil {
					closeAll()
					return
				}
			}
			if err != nil {
				closeAll()
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
				envelope, err := buildPortEnvelope(portMsgControl, mustJSON(portForwardControlMsg{}), sendKey)
				if err != nil {
					continue
				}
				if err := writeFrame(stream, envelope); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := readFrame(stream)
		if err != nil {
			return
		}
		kind, plaintext, err := decryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == portMsgData {
			if _, err := tcpConn.Write(plaintext); err != nil {
				return
			}
		}
		// portMsgControl here is always a ping; nothing else to do with it.
	}
}

// relayPortForwardP2P is relayPortForwardQUIC's WebRTC counterpart: msgCh/
// closed come from the caller's key exchange + OnMessage setup, exactly as
// serveShellChannel's do.
func relayPortForwardP2P(channel *webrtc.DataChannel, tcpConn net.Conn, sendKey, receiveKey []byte,
	msgCh chan []byte, closed <-chan struct{}) {
	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			close(done)
			_ = tcpConn.Close()
			_ = channel.Close()
		})
	}
	defer closeAll()

	SafeGo(func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				envelope, encErr := buildPortEnvelope(portMsgData, buf[:n], sendKey)
				if encErr != nil {
					closeAll()
					return
				}
				if sendErr := channel.Send(envelope); sendErr != nil {
					closeAll()
					return
				}
			}
			if err != nil {
				closeAll()
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
			case <-closed:
				return
			case <-ticker.C:
				envelope, err := buildPortEnvelope(portMsgControl, mustJSON(portForwardControlMsg{}), sendKey)
				if err != nil {
					continue
				}
				if err := channel.Send(envelope); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	for {
		frame, err := recvPriority(msgCh, closed, shellIdleTimeout)
		if err != nil {
			return
		}
		kind, plaintext, err := decryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == portMsgData {
			if _, err := tcpConn.Write(plaintext); err != nil {
				return
			}
		}
	}
}

// handleQUICPortForwardStream serves one forwarded TCP connection over a
// raw QUIC stream: key exchange, read the client's requested port, dial it
// locally, ack (or report the dial error), then relay.
func (d *Deskconn) handleQUICPortForwardStream(stream net.Conn) {
	defer stream.Close()

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
	frame, err := readFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := decryptEnvelope(frame, receiveKey)
	if err != nil || kind != portMsgControl {
		return
	}
	var ctrl portForwardControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		return
	}

	tcpConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", ctrl.Port))
	ack := portForwardControlMsg{}
	if dialErr != nil {
		ack.Error = dialErr.Error()
	}
	if env, buildErr := buildPortEnvelope(portMsgControl, mustJSON(ack), sendKey); buildErr == nil {
		_ = writeFrame(stream, env)
	}
	if dialErr != nil {
		return
	}

	relayPortForwardQUIC(stream, tcpConn, sendKey, receiveKey)
}

// HandlePortForwardChannel serves one forwarded TCP connection over a raw
// WebRTC data channel, mirroring handleQUICPortForwardStream.
func (d *Deskconn) HandlePortForwardChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	SafeGo(func() { d.servePortForwardChannel(channel, firstMessage) })
}

func (d *Deskconn) servePortForwardChannel(channel *webrtc.DataChannel, firstMessage []byte) {
	sendKey, receiveKey, err := p2pServerKeyExchange(channel, firstMessage)
	if err != nil {
		_ = channel.Close()
		return
	}

	closed, _ := webrtcBackpressure(channel)
	msgCh := make(chan []byte, 8)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case msgCh <- msg.Data:
		case <-closed:
		}
	})

	first, err := recvPriority(msgCh, closed, p2pRequestTimeout)
	if err != nil {
		_ = channel.Close()
		return
	}
	_, plaintext, err := decryptEnvelope(first, receiveKey)
	if err != nil {
		_ = channel.Close()
		return
	}
	var ctrl portForwardControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		_ = channel.Close()
		return
	}

	tcpConn, dialErr := net.Dial("tcp", net.JoinHostPort("localhost", ctrl.Port))
	ack := portForwardControlMsg{}
	if dialErr != nil {
		ack.Error = dialErr.Error()
	}
	_ = sendEncryptedJSONPort(channel, ack, sendKey)
	if dialErr != nil {
		_ = channel.Close()
		return
	}

	relayPortForwardP2P(channel, tcpConn, sendKey, receiveKey, msgCh, closed)
}

// sendEncryptedJSONPort mirrors sendEncryptedJSON (filestreamencryption.go)
// with port forward/reverse's own kind byte.
func sendEncryptedJSONPort(channel *webrtc.DataChannel, v any, key []byte) error {
	envelope, err := buildPortEnvelope(portMsgControl, mustJSON(v), key)
	if err != nil {
		return err
	}
	return channel.Send(envelope)
}

// portReverseWriter serializes every send on a session's shared
// stream/channel through one owner. Since QUIC streams and WebRTC reliable-
// ordered channels deliver bytes in write order, funneling all sends
// through a single writer gives per-connection ordering for free -- no
// seq/reorder-buffer needed.
type portReverseWriter struct {
	ch   chan []byte
	done <-chan struct{}
}

func (w *portReverseWriter) send(envelope []byte) bool {
	select {
	case w.ch <- envelope:
		return true
	case <-w.done:
		return false
	}
}

// handleQUICPortReverseStream serves one port-reverse session over a raw
// QUIC stream: key exchange, read the client's listen request, start
// listening, then multiplex every accepted connection's lifecycle and data
// over this one stream (see portReverseWriter).
func (d *Deskconn) handleQUICPortReverseStream(stream net.Conn) {
	defer stream.Close()

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
	frame, err := readFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := decryptEnvelope(frame, receiveKey)
	if err != nil || kind != portMsgControl {
		return
	}
	var ctrl portReverseMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != portRevOpListen {
		return
	}

	ln, listenErr := net.Listen("tcp", ":"+ctrl.RemotePort)
	ack := portReverseMsg{Op: portRevOpListen}
	if listenErr != nil {
		ack.Error = listenErr.Error()
	}
	if env, buildErr := buildPortEnvelope(portMsgControl, mustJSON(ack), sendKey); buildErr == nil {
		_ = writeFrame(stream, env)
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

	writer := &portReverseWriter{ch: make(chan []byte, 64), done: done}
	SafeGo(func() {
		for {
			select {
			case env := <-writer.ch:
				if err := writeFrame(stream, env); err != nil {
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
				env, err := buildPortEnvelope(portMsgControl, mustJSON(portReverseMsg{}), sendKey)
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

			env, buildErr := buildPortEnvelope(portMsgControl,
				mustJSON(portReverseMsg{Op: portRevOpConnect, ConnID: connID}), sendKey)
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
						dataEnv, encErr := buildPortEnvelope(portMsgData, encodeConnData(connID, buf[:n]), sendKey)
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
					closeEnv, encErr := buildPortEnvelope(portMsgControl,
						mustJSON(portReverseMsg{Op: portRevOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.send(closeEnv)
					}
				}
			})
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := readFrame(stream)
		if err != nil {
			return
		}
		kind, plaintext, err := decryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case portMsgControl:
			var msg portReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != portRevOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case portMsgData:
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

// HandlePortReverseChannel serves one port-reverse session over a raw
// WebRTC data channel, mirroring handleQUICPortReverseStream.
func (d *Deskconn) HandlePortReverseChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	SafeGo(func() { d.servePortReverseChannel(channel, firstMessage) })
}

func (d *Deskconn) servePortReverseChannel(channel *webrtc.DataChannel, firstMessage []byte) {
	sendKey, receiveKey, err := p2pServerKeyExchange(channel, firstMessage)
	if err != nil {
		_ = channel.Close()
		return
	}

	closed, _ := webrtcBackpressure(channel)
	msgCh := make(chan []byte, 32)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case msgCh <- msg.Data:
		case <-closed:
		}
	})

	first, err := recvPriority(msgCh, closed, p2pRequestTimeout)
	if err != nil {
		_ = channel.Close()
		return
	}
	_, plaintext, err := decryptEnvelope(first, receiveKey)
	if err != nil {
		_ = channel.Close()
		return
	}
	var ctrl portReverseMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil || ctrl.Op != portRevOpListen {
		_ = channel.Close()
		return
	}

	ln, listenErr := net.Listen("tcp", ":"+ctrl.RemotePort)
	ack := portReverseMsg{Op: portRevOpListen}
	if listenErr != nil {
		ack.Error = listenErr.Error()
	}
	_ = sendEncryptedJSONPort(channel, ack, sendKey)
	if listenErr != nil {
		_ = channel.Close()
		return
	}
	defer ln.Close()

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
				env, err := buildPortEnvelope(portMsgControl, mustJSON(portReverseMsg{}), sendKey)
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

			env, buildErr := buildPortEnvelope(portMsgControl,
				mustJSON(portReverseMsg{Op: portRevOpConnect, ConnID: connID}), sendKey)
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
						dataEnv, encErr := buildPortEnvelope(portMsgData, encodeConnData(connID, buf[:n]), sendKey)
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
					closeEnv, encErr := buildPortEnvelope(portMsgControl,
						mustJSON(portReverseMsg{Op: portRevOpClose, ConnID: connID}), sendKey)
					if encErr == nil {
						writer.send(closeEnv)
					}
				}
			})
		}
	})

	for {
		frame, err := recvPriority(msgCh, closed, shellIdleTimeout)
		if err != nil {
			return
		}
		kind, plaintext, err := decryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case portMsgControl:
			var msg portReverseMsg
			if json.Unmarshal(plaintext, &msg) != nil || msg.Op != portRevOpClose {
				continue
			}
			mu.Lock()
			conn, ok := active[msg.ConnID]
			delete(active, msg.ConnID)
			mu.Unlock()
			if ok {
				conn.Close()
			}
		case portMsgData:
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
