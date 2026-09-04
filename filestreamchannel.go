package deskconn

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

const (
	// fileStreamChunkSize is the size of each binary data channel message
	// used while streaming a byte range, in either direction.
	fileStreamChunkSize = 16 * 1024 // 16KB

	// fileStreamMaxBuffered/fileStreamBufferedLow mirror the backpressure
	// thresholds used for the main WAMP peer in xconn-webrtc-go's peer.go, so a
	// slow reader can't make the send buffer grow unbounded.
	fileStreamMaxBuffered = 512 * 1024 // 512KB
	fileStreamBufferedLow = 256 * 1024 // 256KB

	// fileStreamRequestTimeout bounds how long a write handler waits between
	// binary messages before giving up on a stalled sender.
	fileStreamRequestTimeout = 10 * time.Second

	// fileStreamSessionIdleTimeout bounds how long a read/write channel
	// waits for the next chunk request from its worker before giving up and
	// closing -- normally the worker either sends another request right away
	// or closes the channel itself once its share of the transfer is done.
	fileStreamSessionIdleTimeout = 30 * time.Second
)

// HandleFileStreamChannel is the file-transfer entry point wired into
// HandleAuxDataChannel: every raw (non-WAMP) data channel opened for a file
// transfer -- list/read for downloads, init/write for uploads -- lands here.
// firstMessage is the channel's request frame, already consumed by
// xconn-webrtc-go to classify the channel as non-WAMP before handing it to
// us; it will not be redelivered via channel.OnMessage, so it's parsed
// directly instead of waiting to receive it again.
func (d *Deskconn) HandleFileStreamChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	SafeGo(func() { serveFileStreamChannel(channel, firstMessage) })
}

// serveFileStreamChannel dispatches the channel's first request. list and
// init are one-shot control requests, sent once per whole transfer, so they
// reply and close immediately. read and write are handed to
// serveFileStreamSession, which keeps the channel open across many chunk
// requests -- one parallel worker's share of a transfer can be dozens or
// hundreds of chunks, and reopening a channel per chunk was measured to
// badly limit throughput on real (non-loopback) links: the connection never
// reaches steady-state flow control before it's torn down again.
func serveFileStreamChannel(channel *webrtc.DataChannel, firstMessage []byte) {
	var req fsRequest
	if err := json.Unmarshal(firstMessage, &req); err != nil {
		log.Debugf("filestream: invalid request: %v", err)
		_ = channel.Close()
		return
	}

	switch req.Op {
	case fsOpList:
		serveWebRTCList(channel, req)
	case fsOpInit:
		serveWebRTCInit(channel, req)
	case fsOpRead, fsOpWrite:
		serveFileStreamSession(channel, req)
	default:
		log.Debugf("filestream: unknown op %q", req.Op)
		_ = channel.Close()
	}
}

func sendWebRTCJSON(channel *webrtc.DataChannel, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return channel.SendText(string(b))
}

// remoteRootAndBase resolves a remote transfer's root argument (relative to
// the device's home directory, like every other remote path in this
// package) and returns both the resolved root and its parent, the anchor
// every entry's RelPath is relative to.
func remoteRootAndBase(rootArg string) (root, base string, err error) {
	root, err = resolvePath(rootArg)
	if err != nil {
		return "", "", err
	}
	return root, filepath.Dir(root), nil
}

func serveWebRTCList(channel *webrtc.DataChannel, req fsRequest) {
	defer channel.Close()

	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return
	}

	entries, err := buildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return
	}

	_ = sendWebRTCJSON(channel, fsResponse{OK: true, Entries: entries})
}

func serveWebRTCInit(channel *webrtc.DataChannel, req fsRequest) {
	defer channel.Close()

	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return
	}

	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := materializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return
	}

	_ = sendWebRTCJSON(channel, fsResponse{OK: true})
}

// webrtcBackpressure wires up the OnClose/OnError/OnBufferedAmountLow
// callbacks shared by anything streaming a lot of binary messages over
// channel, in either direction.
func webrtcBackpressure(channel *webrtc.DataChannel) (closed <-chan struct{}, sendReady <-chan struct{}) {
	closedCh := make(chan struct{})
	var closeOnce sync.Once
	markClosed := func() { closeOnce.Do(func() { close(closedCh) }) }
	channel.OnClose(markClosed)
	channel.OnError(func(err error) {
		log.Debugf("filestream: channel error: %v", err)
		markClosed()
	})

	readyCh := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(fileStreamBufferedLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	return closedCh, readyCh
}

// sendWebRTCBytes writes data to channel as fileStreamChunkSize messages,
// blocking on sendReady/closed whenever the channel's send buffer is over
// fileStreamMaxBuffered.
func sendWebRTCBytes(channel *webrtc.DataChannel, closed, sendReady <-chan struct{}, data []byte) error {
	for len(data) > 0 {
		select {
		case <-closed:
			return io.ErrClosedPipe
		default:
		}

		n := fileStreamChunkSize
		if n > len(data) {
			n = len(data)
		}

		for channel.BufferedAmount()+uint64(n) > fileStreamMaxBuffered {
			select {
			case <-sendReady:
			case <-closed:
				return io.ErrClosedPipe
			}
		}

		if err := channel.Send(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// serveFileStreamSession serves req and then any further read/write
// requests the client sends on the same channel, one at a time, letting a
// single parallel worker reuse one channel -- and its SCTP flow-control
// state -- across every chunk it's assigned instead of paying a fresh
// open/close per chunk.
func serveFileStreamSession(channel *webrtc.DataChannel, req fsRequest) {
	defer channel.Close()

	closed, sendReady := webrtcBackpressure(channel)
	reqCh := make(chan fsRequest, 1)
	dataCh := make(chan []byte, 4)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			var next fsRequest
			if json.Unmarshal(msg.Data, &next) == nil {
				select {
				case reqCh <- next:
				case <-closed:
				}
			}
			return
		}
		select {
		case dataCh <- msg.Data:
		case <-closed:
		}
	})

	for {
		var err error
		switch req.Op {
		case fsOpRead:
			err = serveWebRTCReadOnce(channel, closed, sendReady, req)
		case fsOpWrite:
			err = serveWebRTCWriteOnce(channel, closed, dataCh, req)
		default:
			return
		}
		if err != nil {
			return
		}

		next, err := recvPriority(reqCh, closed, fileStreamSessionIdleTimeout)
		if err != nil {
			return
		}
		req = next
	}
}

// serveWebRTCReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// fsResponse.Err and treated as handled.
func serveWebRTCReadOnce(channel *webrtc.DataChannel, closed, sendReady <-chan struct{}, req fsRequest) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return nil
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return nil
	}
	defer f.Close()

	if req.Offset > 0 {
		if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
			_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
			return nil
		}
	}

	if err := sendWebRTCJSON(channel, fsResponse{OK: true}); err != nil {
		return err
	}

	remaining := req.Length
	buf := make([]byte, fileStreamChunkSize)
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, readErr := f.Read(buf[:toRead])
		if n > 0 {
			if err := sendWebRTCBytes(channel, closed, sendReady, buf[:n]); err != nil {
				return err
			}
			remaining -= int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Debugf("filestream: read error: %v", readErr)
			return readErr
		}
	}
	return nil
}

// serveWebRTCWriteOnce serves one byte-range write. Same error-return
// convention as serveWebRTCReadOnce.
func serveWebRTCWriteOnce(channel *webrtc.DataChannel, closed <-chan struct{}, dataCh <-chan []byte,
	req fsRequest) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return nil
	}

	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// A single chunk request never carries the whole manifest, but the
	// sourceRoot argument only matters when rootIsDir is false, and that
	// case only ever has one (non-recursive) entry -- whose RelPath is
	// trivially its own sourceRoot.
	dest := resolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		_ = sendWebRTCJSON(channel, fsResponse{Err: err.Error()})
		return nil
	}
	defer f.Close()

	if err := sendWebRTCJSON(channel, fsResponse{OK: true}); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	var received int64
	for received < req.Length {
		data, err := recvPriority(dataCh, closed, fileStreamRequestTimeout)
		if err != nil {
			log.Debugf("filestream: write stopped after %d/%d bytes: %v", received, req.Length, err)
			return err
		}
		n, err := ow.Write(data)
		received += int64(n)
		if err != nil {
			return err
		}
	}

	return sendWebRTCJSON(channel, fsResponse{OK: true})
}
