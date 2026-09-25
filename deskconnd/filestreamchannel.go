package deskconnd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
)

// HandleFileStreamChannel is the file-transfer entry point for every raw
// (non-WAMP) data channel opened for a file transfer -- list/read for
// downloads, init/write for uploads. firstMessage is the client's plaintext
// ephemeral public key that opens the per-channel key exchange, so it's
// parsed directly rather than waiting to receive it again.
func (d *Deskconn) HandleFileStreamChannel(_ string, channel common.MessageChannel, firstMessage []byte) {
	common.SafeGo(func() { serveFileStreamChannel(channel, firstMessage) })
}

// serveFileStreamChannel performs the per-channel key exchange and
// dispatches the client's first real request, which arrives encrypted
// as the next message. list and init are one-shot requests that reply
// and close immediately. read and write go to serveFileStreamSession,
// which keeps the channel open across many chunk requests -- reopening
// a channel per chunk was measured to badly limit throughput on real (non-loopback) links.
func serveFileStreamChannel(channel common.MessageChannel, firstMessage []byte) {
	sendKey, receiveKey, err := P2PServerKeyExchange(channel, firstMessage)
	if err != nil {
		log.Debugf("filestream: key exchange failed: %v", err)
		_ = channel.Close()
		return
	}

	closed, sendReady := common.WebrtcBackpressure(channel)
	reqCh := make(chan common.FSRequest, 1)
	dataCh := make(chan []byte, 4)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		kind, plaintext, err := common.DecryptEnvelope(msg.Data, receiveKey)
		if err != nil {
			return
		}
		switch kind {
		case common.P2PMsgControl:
			var next common.FSRequest
			if json.Unmarshal(plaintext, &next) == nil {
				select {
				case reqCh <- next:
				case <-closed:
				}
			}
		case common.P2PMsgData:
			select {
			case dataCh <- plaintext:
			case <-closed:
			}
		}
	})

	req, err := common.RecvPriority(reqCh, closed, common.P2PRequestTimeout)
	if err != nil {
		_ = channel.Close()
		return
	}

	switch req.Op {
	case common.FSOpList:
		serveWebRTCList(channel, req, sendKey)
	case common.FSOpInit:
		serveWebRTCInit(channel, req, sendKey)
	case common.FSOpRead, common.FSOpWrite:
		serveFileStreamSession(channel, req, sendKey, closed, sendReady, reqCh, dataCh)
	default:
		log.Debugf("filestream: unknown op %q", req.Op)
		_ = channel.Close()
	}
}

// remoteRootAndBase resolves a remote transfer's root argument
// and returns both the resolved root and its parent, the anchor
// every entry's RelPath is relative to.
func remoteRootAndBase(rootArg string) (root, base string, err error) {
	root, err = resolvePath(rootArg)
	if err != nil {
		return "", "", err
	}
	return root, filepath.Dir(root), nil
}

func serveWebRTCList(channel common.MessageChannel, req common.FSRequest, sendKey []byte) {
	defer channel.Close()

	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}

	entries, err := common.BuildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}

	_ = common.SendEncryptedJSON(channel, common.FSResponse{OK: true, Entries: entries}, sendKey)
}

func serveWebRTCInit(channel common.MessageChannel, req common.FSRequest, sendKey []byte) {
	defer channel.Close()

	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}

	rootIsDir := common.IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := common.MaterializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return
	}

	_ = common.SendEncryptedJSON(channel, common.FSResponse{OK: true}, sendKey)
}

// serveFileStreamSession serves req and then any further read/write
// requests the client sends on the same channel, one at a time, letting a
// single parallel worker reuse one channel across every chunk it's
// assigned. closed/sendReady/reqCh/dataCh come from serveFileStreamChannel,
// which already needed the channel's OnMessage handler set up to read the
// key exchange and req.
func serveFileStreamSession(channel common.MessageChannel, req common.FSRequest, sendKey []byte,
	closed, sendReady <-chan struct{}, reqCh <-chan common.FSRequest, dataCh <-chan []byte) {
	defer channel.Close()

	for {
		var err error
		switch req.Op {
		case common.FSOpRead:
			err = serveWebRTCReadOnce(channel, closed, sendReady, req, sendKey)
		case common.FSOpWrite:
			err = serveWebRTCWriteOnce(channel, closed, dataCh, req, sendKey)
		default:
			return
		}
		if err != nil {
			return
		}

		next, err := common.RecvPriority(reqCh, closed, common.FileStreamSessionIdleTimeout)
		if err != nil {
			return
		}
		req = next
	}
}

// serveWebRTCReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// FSResponse.Err and treated as handled.
func serveWebRTCReadOnce(channel common.MessageChannel, closed, sendReady <-chan struct{}, req common.FSRequest,
	sendKey []byte) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return nil
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return nil
	}
	defer f.Close()

	if req.Offset > 0 {
		if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
			_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
			return nil
		}
	}

	if err := common.SendEncryptedJSON(channel, common.FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	remaining := req.Length
	buf := make([]byte, common.FileStreamChunkSize)
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, readErr := f.Read(buf[:toRead])
		if n > 0 {
			if err := common.SendEncryptedBytes(channel, closed, sendReady, buf[:n], sendKey); err != nil {
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
func serveWebRTCWriteOnce(channel common.MessageChannel, closed <-chan struct{}, dataCh <-chan []byte,
	req common.FSRequest, sendKey []byte) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return nil
	}

	rootIsDir := common.IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// A single chunk request never carries the whole manifest, but the
	// sourceRoot argument only matters when rootIsDir is false, and that
	// case only ever has one (non-recursive) entry -- whose RelPath is
	// trivially its own sourceRoot.
	dest := common.ResolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		_ = common.SendEncryptedJSON(channel, common.FSResponse{Err: err.Error()}, sendKey)
		return nil
	}
	defer f.Close()

	if err := common.SendEncryptedJSON(channel, common.FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	var received int64
	for received < req.Length {
		data, err := common.RecvPriority(dataCh, closed, common.FileStreamRequestTimeout)
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

	return common.SendEncryptedJSON(channel, common.FSResponse{OK: true}, sendKey)
}
