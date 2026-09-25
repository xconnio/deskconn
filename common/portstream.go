package common

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// BuildPortEnvelope encrypts plaintext and prepends kind, ready to hand to
// either transport's raw send.
func BuildPortEnvelope(kind byte, plaintext, key []byte) ([]byte, error) {
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 1+len(ciphertext))
	envelope[0] = kind
	copy(envelope[1:], ciphertext)
	return envelope, nil
}

// PortForwardControlMsg is port forward's only control message: the
// client's "dial this port" request, the device's ack/Error, or -- both
// fields empty -- a ping keeping ShellIdleTimeout from firing on a quiet
// connection.
type PortForwardControlMsg struct {
	Port  string `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

// EncodeConnData/DecodeConnData tag a port-reverse data chunk with which
// forwarded connection it belongs to: 8-byte big-endian connID, then the
// raw payload. Binary rather than JSON so per-chunk overhead stays minimal
// on what can be high-volume traffic.
func EncodeConnData(connID uint64, payload []byte) []byte {
	buf := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint64(buf, connID)
	copy(buf[8:], payload)
	return buf
}

func DecodeConnData(data []byte) (connID uint64, payload []byte, ok bool) {
	if len(data) < 8 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64(data), data[8:], true
}

// portReverseOp is PortReverseMsg's discriminator.
type portReverseOp string

// PortReverseMsg carries every port-reverse control message: connection
// lifecycle events multiplexed over one session stream. Data itself travels
// as PortMsgData envelopes tagged via EncodeConnData, not through this
// struct. Op == "" is a ping.
type PortReverseMsg struct {
	Op         portReverseOp `json:"op,omitempty"`
	ConnID     uint64        `json:"conn_id,omitempty"`
	RemotePort string        `json:"remote_port,omitempty"` // listen only
	Error      string        `json:"error,omitempty"`       // listen ack failure
}

// RelayPortForwardQUIC bidirectionally relays tcpConn's bytes over stream,
// encrypted, until either side closes or goes idle past ShellIdleTimeout.
// Used by both the device (relaying to the dialed backend) and the client
// (relaying to the locally accepted connection) -- the relay itself doesn't
// care which side opened the stream, so one implementation serves both.
func RelayPortForwardQUIC(stream net.Conn, tcpConn net.Conn, sendKey, receiveKey []byte) {
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
				envelope, encErr := BuildPortEnvelope(PortMsgData, buf[:n], sendKey)
				if encErr != nil {
					closeAll()
					return
				}
				if wErr := WriteFrame(stream, envelope); wErr != nil {
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
		ticker := time.NewTicker(ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				envelope, err := BuildPortEnvelope(PortMsgControl, MustJSON(PortForwardControlMsg{}), sendKey)
				if err != nil {
					continue
				}
				if err := WriteFrame(stream, envelope); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	for {
		_ = stream.SetReadDeadline(time.Now().Add(ShellIdleTimeout))
		frame, err := ReadFrame(stream)
		if err != nil {
			return
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == PortMsgData {
			if _, err := tcpConn.Write(plaintext); err != nil {
				return
			}
		}
		// PortMsgControl here is always a ping; nothing else to do with it.
	}
}

// RelayPortForwardP2P is RelayPortForwardQUIC's WebRTC counterpart: msgCh/
// closed come from the caller's key exchange + OnMessage setup, exactly as
// the shell channel handler's do.
func RelayPortForwardP2P(channel MessageChannel, tcpConn net.Conn, sendKey, receiveKey []byte,
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
				envelope, encErr := BuildPortEnvelope(PortMsgData, buf[:n], sendKey)
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
		ticker := time.NewTicker(ShellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-closed:
				return
			case <-ticker.C:
				envelope, err := BuildPortEnvelope(PortMsgControl, MustJSON(PortForwardControlMsg{}), sendKey)
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
		frame, err := RecvPriority(msgCh, closed, ShellIdleTimeout)
		if err != nil {
			return
		}
		kind, plaintext, err := DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		if kind == PortMsgData {
			if _, err := tcpConn.Write(plaintext); err != nil {
				return
			}
		}
	}
}

// PortReverseWriter serializes every send on a session's shared
// stream/channel through one owner. Since QUIC streams and WebRTC reliable-
// ordered channels deliver bytes in write order, funneling all sends
// through a single writer gives per-connection ordering for free -- no
// seq/reorder-buffer needed. Shared by port-reverse and agent-forward,
// both client and server sides.
type PortReverseWriter struct {
	Ch   chan []byte
	Done <-chan struct{}
}

func (w *PortReverseWriter) Send(envelope []byte) bool {
	select {
	case w.Ch <- envelope:
		return true
	case <-w.Done:
		return false
	}
}
