package deskconnd

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

	"github.com/xconnio/deskconn/common"
)

// newShellStreamID generates a random ID for a new shell connection. No
// rekeying is needed on migration -- the ID just stays the same across the
// transport swap.
func newShellStreamID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type quicShellTransport struct {
	stream  net.Conn
	sendKey []byte
}

func (t *quicShellTransport) writeOutput(plaintext []byte) error {
	envelope, err := common.BuildShellEnvelope(common.ShellMsgData, plaintext, t.sendKey)
	if err != nil {
		return err
	}
	return common.WriteFrame(t.stream, envelope)
}

func (t *quicShellTransport) close() error { return t.stream.Close() }

type p2pShellTransport struct {
	channel common.MessageChannel
	sendKey []byte
}

func (t *p2pShellTransport) writeOutput(plaintext []byte) error {
	envelope, err := common.BuildShellEnvelope(common.ShellMsgData, plaintext, t.sendKey)
	if err != nil {
		return err
	}
	return t.channel.Send(envelope)
}

func (t *p2pShellTransport) close() error { return t.channel.Close() }

// beginShellSession handles a connection's first control message: claims an
// existing PTY via a valid migration token (ShellOpMigrate), or
// creates a new one (ShellOpSize) running ctrl.Command (or an
// interactive bash shell if empty -- this same protocol serves both `shell`
// and `exec`), issuing a fresh migration token for it.
// Returns a non-nil error if the request was invalid or the command failed
// to start (e.g. exec given a nonexistent command) -- the caller should
// report it to the client and then close the connection. On success, the
// caller must call startReader only after it has sent the ack: for a fresh
// session this is what starts the PTY's output reader, and calling it any
// earlier would let output race ahead of (and be mistaken by the client
// for) the ack itself. Migrating a session returns a no-op startReader,
// since its output reader is already running from before. Shared by both
// transports.
func (p *interactiveShellSession) beginShellSession(ctrl common.ShellControlMsg, transport shellTransport) (
	shellID, migrationToken string, ptmx *os.File, startReader func(), err error) {
	if ctrl.Op == common.ShellOpMigrate {
		p.Lock()
		expected, tokenOK := p.migrationTokens[ctrl.OldID]
		ps, psOK := p.sessions[ctrl.OldID]
		existingPtmx, ptmxOK := p.ptmx[ctrl.OldID]
		p.Unlock()

		valid := tokenOK && psOK && ptmxOK && time.Since(expected.issuedAt) <= migrationTokenTTL &&
			subtle.ConstantTimeCompare([]byte(ctrl.Token), []byte(expected.value)) == 1
		if !valid {
			return "", "", nil, nil, fmt.Errorf("invalid or expired migration token")
		}

		p.Lock()
		delete(p.migrationTokens, ctrl.OldID)
		p.Unlock()
		ps.mu.Lock()
		ps.transport = transport
		ps.mu.Unlock()
		return ctrl.OldID, "", existingPtmx, func() {}, nil
	}

	interactive := ctrl.Command == ""
	command := ctrl.Command
	if command == "" {
		command = "bash"
	}
	shellID = newShellStreamID()
	ws := &pty.Winsize{Cols: ctrl.Cols, Rows: ctrl.Rows}
	newPt, startReader, err := p.startPtySession(
		transport, shellID, p.agentSockForAuthID(ctrl.AuthID), "", command, ws, interactive, ctrl.Args...)
	if err != nil {
		return "", "", nil, nil, err
	}
	return shellID, p.issueMigrationToken(shellID), newPt, startReader, nil
}

// handleQUICShellStream serves one shell over a raw QUIC stream: key
// exchange, beginShellSession on the first control message, then a read
// loop dispatching every further control/data envelope until it closes.
func (d *Deskconn) handleQUICShellStream(stream net.Conn) {
	defer stream.Close()

	_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	frame, err := common.ReadFrame(stream)
	if err != nil {
		return
	}
	kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
	if err != nil || kind != common.ShellMsgControl {
		return
	}
	var ctrl common.ShellControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		return
	}

	transport := &quicShellTransport{stream: stream, sendKey: sendKey}
	shellID, token, ptmx, startReader, err := d.shellSession.beginShellSession(ctrl, transport)
	ackMsg := common.ShellControlMsg{ShellID: shellID, Token: token}
	if err != nil {
		ackMsg = common.ShellControlMsg{Error: err.Error()}
	}
	if ack, buildErr := common.BuildShellEnvelope(common.ShellMsgControl,
		common.MustJSON(ackMsg), sendKey); buildErr == nil {
		_ = common.WriteFrame(stream, ack)
	}
	if err != nil {
		return
	}
	startReader()

	for {
		_ = stream.SetReadDeadline(time.Now().Add(common.ShellIdleTimeout))
		frame, err := common.ReadFrame(stream)
		if err != nil {
			d.shellSession.endShellInput(shellID, transport)
			return
		}
		kind, plaintext, err := common.DecryptEnvelope(frame, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case common.ShellMsgControl:
			var next common.ShellControlMsg
			if json.Unmarshal(plaintext, &next) == nil && next.Op == common.ShellOpSize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: next.Cols, Rows: next.Rows})
			}
		case common.ShellMsgData:
			_, _ = ptmx.Write(plaintext)
		}
	}
}

// HandleShellChannel serves one shell over a raw WebRTC data channel,
// mirroring handleQUICShellStream.
func (d *Deskconn) HandleShellChannel(_ string, channel common.MessageChannel, firstMessage []byte) {
	common.SafeGo(func() { d.serveShellChannel(channel, firstMessage) })
}

func (d *Deskconn) serveShellChannel(channel common.MessageChannel, firstMessage []byte) {
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
	var ctrl common.ShellControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		_ = channel.Close()
		return
	}

	transport := &p2pShellTransport{channel: channel, sendKey: sendKey}
	shellID, token, ptmx, startReader, err := d.shellSession.beginShellSession(ctrl, transport)
	ackMsg := common.ShellControlMsg{ShellID: shellID, Token: token}
	if err != nil {
		ackMsg = common.ShellControlMsg{Error: err.Error()}
	}
	_ = sendEncryptedJSONShell(channel, ackMsg, sendKey)
	if err != nil {
		_ = channel.Close()
		return
	}
	startReader()

	for {
		data, err := common.RecvPriority(msgCh, closed, common.FileStreamSessionIdleTimeout)
		if err != nil {
			d.shellSession.endShellInput(shellID, transport)
			return
		}
		kind, plaintext, err := common.DecryptEnvelope(data, receiveKey)
		if err != nil {
			continue
		}
		switch kind {
		case common.ShellMsgControl:
			var next common.ShellControlMsg
			if json.Unmarshal(plaintext, &next) == nil && next.Op == common.ShellOpSize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: next.Cols, Rows: next.Rows})
			}
		case common.ShellMsgData:
			_, _ = ptmx.Write(plaintext)
		}
	}
}

// sendEncryptedJSONShell mirrors SendEncryptedJSON with shell's own
// kind byte.
func sendEncryptedJSONShell(channel common.MessageChannel, v any, key []byte) error {
	envelope, err := common.BuildShellEnvelope(common.ShellMsgControl, common.MustJSON(v), key)
	if err != nil {
		return err
	}
	return channel.Send(envelope)
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
