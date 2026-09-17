package deskconn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/creack/pty"
	"github.com/pion/webrtc/v4"
)

// newShellStreamID generates a random ID for a new shell connection. No
// rekeying is needed on migration -- the ID just stays the same across the
// transport swap.
func newShellStreamID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// shellChannelLabel is the WebRTC data channel label a shell connection
// opens, checked by HandleAuxDataChannel (iptunnel.go) before the
// first-message classification file transfer uses -- a shell channel's
// first message is a plaintext public key, indistinguishable by content
// alone from a file-stream channel's.
const shellChannelLabel = "shell"

// shellControlOp is shellControlMsg's discriminator.
type shellControlOp string

const (
	// shellOpSize creates a new PTY if this connection has none yet,
	// otherwise resizes the existing one.
	shellOpSize shellControlOp = "size"
	// shellOpMigrate claims an existing PTY from a different transport --
	// sent as the first message on a freshly opened connection.
	shellOpMigrate shellControlOp = "migrate"
	// shellOpPing is sent periodically by the client whenever otherwise
	// idle, purely so the connection keeps producing traffic -- it needs no
	// server-side handling beyond having been read (see shellIdleTimeout).
	shellOpPing shellControlOp = "ping"
)

// shellIdleTimeout bounds how long a shell/exec connection may go without
// any traffic before it's considered dead. QUIC streams have no equivalent
// of WebRTC's own peer-connectivity checks, so without this an abruptly
// disconnected client (crash, killed process, dropped network) leaves its
// remote command running forever instead of being cleaned up -- the client
// sends shellOpPing on a timer (shellPingInterval, shellclient.go)
// specifically to keep this from firing during genuine silence (e.g. a user
// just reading output, not typing).
const shellIdleTimeout = fileStreamSessionIdleTimeout

// shellControlMsg is every control message in the raw-stream shell
// protocol, sent inside the same encrypted envelope (shellMsgControl kind)
// as everything else on the connection.
type shellControlMsg struct {
	Op      shellControlOp `json:"op"`
	Cols    uint16         `json:"cols,omitempty"`
	Rows    uint16         `json:"rows,omitempty"`
	AuthID  string         `json:"auth_id,omitempty"`  // size only: self-reported, for agent-forward lookup
	Command string         `json:"command,omitempty"`  // size only: exec's command; empty means an interactive bash shell
	Args    []string       `json:"args,omitempty"`     // size only: exec's arguments to Command
	OldID   string         `json:"old_id,omitempty"`   // migrate only: shell ID being claimed
	Token   string         `json:"token,omitempty"`    // migrate only: the token issued for OldID
	ShellID string         `json:"shell_id,omitempty"` // server->client ack: the (possibly new) shell ID
	Error   string         `json:"error,omitempty"`    // server->client ack: set instead of ShellID/Token on failure
}

const (
	shellMsgControl byte = iota // encrypted JSON shellControlMsg
	shellMsgData                // encrypted raw PTY input/output bytes
)

// buildShellEnvelope encrypts plaintext and prepends kind, ready to hand to
// either transport's raw send.
func buildShellEnvelope(kind byte, plaintext, key []byte) ([]byte, error) {
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 1+len(ciphertext))
	envelope[0] = kind
	copy(envelope[1:], ciphertext)
	return envelope, nil
}

type quicShellTransport struct {
	stream  net.Conn
	sendKey []byte
}

func (t *quicShellTransport) writeOutput(plaintext []byte) error {
	envelope, err := buildShellEnvelope(shellMsgData, plaintext, t.sendKey)
	if err != nil {
		return err
	}
	return writeFrame(t.stream, envelope)
}

func (t *quicShellTransport) close() error { return t.stream.Close() }

type p2pShellTransport struct {
	channel *webrtc.DataChannel
	sendKey []byte
}

func (t *p2pShellTransport) writeOutput(plaintext []byte) error {
	envelope, err := buildShellEnvelope(shellMsgData, plaintext, t.sendKey)
	if err != nil {
		return err
	}
	return t.channel.Send(envelope)
}

func (t *p2pShellTransport) close() error { return t.channel.Close() }

// beginShellSession handles a connection's first control message: claims an
// existing PTY via a valid migration token (shellOpMigrate), or creates a
// new one (shellOpSize) running ctrl.Command (or an interactive bash shell
// if empty -- this same protocol serves both `shell` and `exec`), issuing a
// fresh migration token for it.
// Returns a non-nil error if the request was invalid or the command failed
// to start (e.g. exec given a nonexistent command) -- the caller should
// report it to the client and then close the connection. Shared by both
// transports.
func (p *interactiveShellSession) beginShellSession(ctrl shellControlMsg, transport shellTransport) (
	shellID, migrationToken string, ptmx *os.File, err error) {
	if ctrl.Op == shellOpMigrate {
		p.Lock()
		expected, tokenOK := p.migrationTokens[ctrl.OldID]
		ps, psOK := p.sessions[ctrl.OldID]
		existingPtmx, ptmxOK := p.ptmx[ctrl.OldID]
		p.Unlock()

		valid := tokenOK && psOK && ptmxOK && time.Since(expected.issuedAt) <= migrationTokenTTL &&
			subtle.ConstantTimeCompare([]byte(ctrl.Token), []byte(expected.value)) == 1
		if !valid {
			return "", "", nil, fmt.Errorf("invalid or expired migration token")
		}

		p.Lock()
		delete(p.migrationTokens, ctrl.OldID)
		p.Unlock()
		ps.mu.Lock()
		ps.transport = transport
		ps.mu.Unlock()
		return ctrl.OldID, "", existingPtmx, nil
	}

	command := ctrl.Command
	if command == "" {
		command = "bash"
	}
	shellID = newShellStreamID()
	ws := &pty.Winsize{Cols: ctrl.Cols, Rows: ctrl.Rows}
	newPt, err := p.startPtySession(transport, shellID, p.agentSockForAuthID(ctrl.AuthID), "", command, ws, ctrl.Args...)
	if err != nil {
		return "", "", nil, err
	}
	return shellID, p.issueMigrationToken(shellID), newPt, nil
}

// handleQUICShellStream serves one shell over a raw QUIC stream: key
// exchange, beginShellSession on the first control message, then a read
// loop dispatching every further control/data envelope until it closes.
func (d *Deskconn) handleQUICShellStream(stream net.Conn) {
	defer stream.Close()

	_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	frame, err := readFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := decryptEnvelope(frame, receiveKey)
	if err != nil || kind != shellMsgControl {
		return
	}
	var ctrl shellControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		return
	}

	transport := &quicShellTransport{stream: stream, sendKey: sendKey}
	shellID, token, ptmx, err := d.shellSession.beginShellSession(ctrl, transport)
	ackMsg := shellControlMsg{ShellID: shellID, Token: token}
	if err != nil {
		ackMsg = shellControlMsg{Error: err.Error()}
	}
	if ack, buildErr := buildShellEnvelope(shellMsgControl, mustJSON(ackMsg), sendKey); buildErr == nil {
		_ = writeFrame(stream, ack)
	}
	if err != nil {
		return
	}

	for {
		_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
		frame, err := readFrame(stream)
		if err != nil {
			d.shellSession.endShellInput(shellID, transport)
			return
		}
		kind, plaintext, err := decryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case shellMsgControl:
			var next shellControlMsg
			if json.Unmarshal(plaintext, &next) == nil && next.Op == shellOpSize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: next.Cols, Rows: next.Rows})
			}
		case shellMsgData:
			_, _ = ptmx.Write(plaintext)
		}
	}
}

// HandleShellChannel serves one shell over a raw WebRTC data channel,
// mirroring handleQUICShellStream.
func (d *Deskconn) HandleShellChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	SafeGo(func() { d.serveShellChannel(channel, firstMessage) })
}

func (d *Deskconn) serveShellChannel(channel *webrtc.DataChannel, firstMessage []byte) {
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
	var ctrl shellControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		_ = channel.Close()
		return
	}

	transport := &p2pShellTransport{channel: channel, sendKey: sendKey}
	shellID, token, ptmx, err := d.shellSession.beginShellSession(ctrl, transport)
	ackMsg := shellControlMsg{ShellID: shellID, Token: token}
	if err != nil {
		ackMsg = shellControlMsg{Error: err.Error()}
	}
	_ = sendEncryptedJSONShell(channel, ackMsg, sendKey)
	if err != nil {
		_ = channel.Close()
		return
	}

	for {
		data, err := recvPriority(msgCh, closed, fileStreamSessionIdleTimeout)
		if err != nil {
			d.shellSession.endShellInput(shellID, transport)
			return
		}
		kind, plaintext, err := decryptEnvelope(data, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case shellMsgControl:
			var next shellControlMsg
			if json.Unmarshal(plaintext, &next) == nil && next.Op == shellOpSize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: next.Cols, Rows: next.Rows})
			}
		case shellMsgData:
			_, _ = ptmx.Write(plaintext)
		}
	}
}

// sendEncryptedJSONShell mirrors sendEncryptedJSON (filestreamencryption.go)
// with shell's own kind byte.
func sendEncryptedJSONShell(channel *webrtc.DataChannel, v any, key []byte) error {
	envelope, err := buildShellEnvelope(shellMsgControl, mustJSON(v), key)
	if err != nil {
		return err
	}
	return channel.Send(envelope)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// endShellInput cleans up the PTY if this transport still owns it (a real
// disconnect); if the session already migrated to another transport, it's
// a no-op.
func (p *interactiveShellSession) endShellInput(shellID string, transport shellTransport) {
	p.Lock()
	ps, ok := p.sessions[shellID]
	p.Unlock()
	if !ok {
		return
	}
	ps.mu.Lock()
	stillOwns := ps.transport == transport
	ps.mu.Unlock()
	if stillOwns {
		p.cleanupShellID(shellID)
	}
}
