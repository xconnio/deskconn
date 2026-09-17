package deskconn

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pion/webrtc/v4"
)

// logChannelLabel is checked by HandleAuxDataChannel (iptunnel.go) before
// file transfer's first-message sniffing, same as shellChannelLabel.
const logChannelLabel = "logs"

// Envelope kind bytes, own namespace like every other raw-stream feature's.
const (
	logMsgControl byte = iota // encrypted JSON logControlMsg, client's one-time request
	logMsgData                // encrypted log bytes, device -> client
	logMsgPing                // no payload, either direction, keeps shellIdleTimeout from firing
)

// logControlMsg is the client's one-time request, sent right after key
// exchange. There's no ack: the device just starts streaming (or streams a
// single "error: ...\n" chunk, then closes) immediately, matching the
// pre-migration behavior of reporting failures as regular log output.
type logControlMsg struct {
	Source string `json:"source,omitempty"`
	Follow bool   `json:"follow,omitempty"`
	TailN  int64  `json:"tail_n,omitempty"`
	Since  string `json:"since,omitempty"`
}

// logSender writes one already-built envelope to the peer, synchronously --
// unlike port-reverse/agent-forward's buffered portReverseWriter, logs only
// ever has one data-producing goroutine (the file/journal reader, running
// synchronously in the handler) plus the ping ticker, so a plain mutex
// around the write is enough to serialize them, and (unlike a buffered
// queue) guarantees every send has actually reached the wire before the
// caller moves on -- important here since the handler closes the stream
// right after its data-producing call returns.
type logSender func(envelope []byte) error

func newLogSender(write func([]byte) error) logSender {
	var mu sync.Mutex
	return func(envelope []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return write(envelope)
	}
}

func sendLogData(send logSender, sendKey []byte, data []byte) bool {
	env, err := buildPortEnvelope(logMsgData, data, sendKey)
	if err != nil {
		return false
	}
	return send(env) == nil
}

// handleQUICLogsStream serves one `deskconn logs` session over a raw QUIC
// stream: key exchange, read the client's one-time request, then stream
// log data until done or the client disconnects.
func (d *Deskconn) handleQUICLogsStream(stream net.Conn) {
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
	if err != nil || kind != logMsgControl {
		return
	}
	var ctrl logControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		return
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	closeAll := func() {
		doneOnce.Do(func() {
			close(done)
			_ = stream.Close()
		})
	}
	defer closeAll()

	send := newLogSender(func(envelope []byte) error { return writeFrame(stream, envelope) })

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(logMsgPing, nil, sendKey)
				if err != nil {
					continue
				}
				if err := send(env); err != nil {
					closeAll()
					return
				}
			}
		}
	})

	SafeGo(func() {
		for {
			_ = stream.SetReadDeadline(time.Now().Add(shellIdleTimeout))
			frame, err := readFrame(stream)
			if err != nil {
				closeAll()
				return
			}
			_, _, _ = decryptEnvelope(frame, receiveKey)
		}
	})

	streamLogsRaw(send, sendKey, done, ctrl)
}

// HandleLogsChannel serves one `deskconn logs` session over a raw WebRTC
// data channel, mirroring handleQUICLogsStream.
func (d *Deskconn) HandleLogsChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	SafeGo(func() { d.serveLogsChannel(channel, firstMessage) })
}

func (d *Deskconn) serveLogsChannel(channel *webrtc.DataChannel, firstMessage []byte) {
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
	var ctrl logControlMsg
	if err := json.Unmarshal(plaintext, &ctrl); err != nil {
		_ = channel.Close()
		return
	}

	send := newLogSender(channel.Send)

	SafeGo(func() {
		ticker := time.NewTicker(shellPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
				env, err := buildPortEnvelope(logMsgPing, nil, sendKey)
				if err != nil {
					continue
				}
				if err := send(env); err != nil {
					_ = channel.Close()
					return
				}
			}
		}
	})

	SafeGo(func() {
		for {
			if _, err := recvPriority(msgCh, closed, shellIdleTimeout); err != nil {
				_ = channel.Close()
				return
			}
		}
	})

	streamLogsRaw(send, sendKey, closed, ctrl)
	_ = channel.Close()
}

// streamLogsRaw validates ctrl.Since (matching handleLogs' old validation,
// still enforced here as defense in depth even though RunLogs already
// rejects an invalid --since before ever connecting), then dispatches to
// the file or journal source, exactly like the old handleLogs did.
func streamLogsRaw(send logSender, sendKey []byte, done <-chan struct{}, ctrl logControlMsg) {
	if ctrl.Since != "" {
		if _, parseErr := time.ParseDuration(ctrl.Since); parseErr != nil {
			sendLogData(send, sendKey, []byte(fmt.Sprintf("error: invalid since value %q: %s\n", ctrl.Since, parseErr.Error())))
			return
		}
	}

	if strings.HasPrefix(ctrl.Source, "/") {
		streamFileLogsRaw(send, sendKey, done, ctrl.Source, ctrl.Follow, ctrl.TailN)
	} else {
		streamJournalLogsRaw(send, sendKey, done, ctrl.Source, ctrl.Follow, ctrl.TailN, ctrl.Since)
	}
}

func isDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// streamJournalLogsRaw manages journalctl's process lifecycle directly
// (Kill on done) rather than via exec.CommandContext/context.Context --
// done is already this stream's own cancellation signal, and there's no
// caller-supplied context.Context available at this point in a raw-stream
// handler to thread through instead (mirrors shell.go's
// killShellProcessGroup, which manages its process the same direct way).
func streamJournalLogsRaw(send logSender, sendKey []byte, done <-chan struct{},
	service string, follow bool, tailN int64, since string) {
	args := journalctlArgs(service, follow, tailN, since)
	cmd := exec.Command("journalctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", err.Error())))
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	if err := cmd.Start(); err != nil {
		sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", err.Error())))
		return
	}

	killDone := make(chan struct{})
	defer close(killDone)
	SafeGo(func() {
		select {
		case <-done:
			_ = cmd.Process.Kill()
		case <-killDone:
		}
	})

	sent := false
	for scanner.Scan() {
		if isDone(done) {
			return
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		tsUsec, fields, parseErr := parseJournalctlEntry(line)
		if parseErr != nil {
			continue
		}

		sent = true
		if !sendLogData(send, sendKey, []byte(formatEntry(tsUsec, fields))) {
			return
		}
	}

	waitErr := cmd.Wait()
	if isDone(done) {
		return
	}

	if err := scanner.Err(); err != nil {
		if stderr.Len() > 0 {
			sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String()))))
			return
		}
		sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", err.Error())))
		return
	}

	if !sent {
		if stderr.Len() > 0 {
			sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String()))))
			return
		}
		if waitErr != nil {
			sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", waitErr.Error())))
			return
		}
		sendLogData(send, sendKey, []byte("-- No entries --\n"))
		return
	}

	if waitErr != nil && stderr.Len() > 0 && !isDone(done) {
		sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", strings.TrimSpace(stderr.String()))))
	}
}

func streamFileLogsRaw(send logSender, sendKey []byte, done <-chan struct{},
	path string, follow bool, tailN int64) {
	f, err := os.Open(path)
	if err != nil {
		sendLogData(send, sendKey, []byte(fmt.Sprintf("error: %s\n", err.Error())))
		return
	}
	defer f.Close()

	n := tailN
	if n < 0 {
		if follow {
			// --follow with no --tail: start from end of file
			if _, seekErr := f.Seek(0, io.SeekEnd); seekErr != nil {
				return
			}
		} else {
			n = 10
		}
	}
	if n >= 0 {
		if offset, offsetErr := nthLineFromEnd(path, n); offsetErr == nil {
			_, _ = f.Seek(offset, io.SeekStart)
		}
	}

	if !follow {
		buf := make([]byte, 4096)
		sent := false
		for {
			select {
			case <-done:
				return
			default:
			}
			nr, readErr := f.Read(buf)
			if nr > 0 {
				sent = true
				chunk := make([]byte, nr)
				copy(chunk, buf[:nr])
				if !sendLogData(send, sendKey, chunk) {
					return
				}
			}
			if readErr != nil {
				if !sent {
					sendLogData(send, sendKey, []byte("-- No entries --\n"))
				}
				return
			}
		}
	}

	watcher, watchErr := fsnotify.NewWatcher()
	if watchErr != nil {
		return
	}
	defer watcher.Close()
	if addErr := watcher.Add(path); addErr != nil {
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, readErr := f.Read(buf)
		if nr > 0 {
			chunk := make([]byte, nr)
			copy(chunk, buf[:nr])
			if !sendLogData(send, sendKey, chunk) {
				return
			}
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return
		}
		select {
		case <-done:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				continue
			}
		case <-watcher.Errors:
			return
		}
	}
}
