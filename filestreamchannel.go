package deskconn

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

const (
	// fileStreamChunkSize is the size of each binary data channel message sent
	// for a range read.
	fileStreamChunkSize = 16 * 1024 // 16KB

	// fileStreamMaxBuffered/fileStreamBufferedLow mirror the backpressure
	// thresholds used for the main WAMP peer in xconn-webrtc-go's peer.go, so a
	// slow reader can't make the send buffer grow unbounded.
	fileStreamMaxBuffered = 512 * 1024 // 512KB
	fileStreamBufferedLow = 256 * 1024 // 256KB

	// fileStreamRequestTimeout bounds how long we hold a channel open waiting for
	// its request frame before giving up and closing it.
	fileStreamRequestTimeout = 10 * time.Second
)

type fileStreamRequest struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

type fileStreamHeader struct {
	Size   int64 `json:"size"`
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// HandleFileStreamChannel must not block, so it defers the actual work to
// serveFileStreamChannel, which serves one byte-range read per channel.
// firstMessage is the channel's request frame, already consumed by
// xconn-webrtc-go to classify the channel as non-WAMP before handing it to
// us; it will not be redelivered via channel.OnMessage, so it's fed to
// serveFileStreamChannel directly instead of waiting to receive it again.
func (d *Deskconn) HandleFileStreamChannel(_ string, channel *webrtc.DataChannel, firstMessage []byte) {
	msgCh := make(chan []byte, 1)
	msgCh <- append([]byte(nil), firstMessage...)

	SafeGo(func() { d.serveFileStreamChannel(channel, msgCh) })
}

func (d *Deskconn) serveFileStreamChannel(channel *webrtc.DataChannel, msgCh chan []byte) {
	var initial []byte
	select {
	case initial = <-msgCh:
	case <-time.After(fileStreamRequestTimeout):
		log.Debugf("filestream: no request received within %s", fileStreamRequestTimeout)
		_ = channel.Close()
		return
	}

	var req fileStreamRequest
	if err := json.Unmarshal(initial, &req); err != nil {
		log.Debugf("filestream: invalid request: %v", err)
		_ = channel.Close()
		return
	}

	resolvedPath, info, invErr := resolveAndStatPath(req.Path)
	if invErr != nil {
		log.Debugf("filestream: cannot resolve %q", req.Path)
		_ = channel.Close()
		return
	}
	if info.IsDir() {
		log.Debugf("filestream: %q is a directory", req.Path)
		_ = channel.Close()
		return
	}

	totalSize := info.Size()
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > totalSize {
		offset = totalSize
	}
	length := req.Length
	if length < 0 || offset+length > totalSize {
		length = totalSize - offset
	}

	f, err := os.Open(resolvedPath)
	if err != nil {
		log.Debugf("filestream: failed to open %s: %v", resolvedPath, err)
		_ = channel.Close()
		return
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			log.Debugf("filestream: seek to %d failed: %v", offset, err)
			_ = channel.Close()
			return
		}
	}

	closed := make(chan struct{})
	var closeOnce sync.Once
	markClosed := func() { closeOnce.Do(func() { close(closed) }) }
	channel.OnClose(func() { markClosed() })
	channel.OnError(func(err error) {
		log.Debugf("filestream: channel error: %v", err)
		markClosed()
	})

	sendReady := make(chan struct{}, 1)
	channel.SetBufferedAmountLowThreshold(fileStreamBufferedLow)
	channel.OnBufferedAmountLow(func() {
		select {
		case sendReady <- struct{}{}:
		default:
		}
	})

	headerBytes, err := json.Marshal(fileStreamHeader{Size: totalSize, Offset: offset, Length: length})
	if err != nil {
		_ = channel.Close()
		return
	}
	if err := channel.SendText(string(headerBytes)); err != nil {
		return
	}

	remaining := length
	buf := make([]byte, fileStreamChunkSize)
	for remaining > 0 {
		select {
		case <-closed:
			return
		default:
		}

		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, readErr := f.Read(buf[:toRead])
		if n > 0 {
			for channel.BufferedAmount()+uint64(n) > fileStreamMaxBuffered {
				select {
				case <-sendReady:
				case <-closed:
					return
				}
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := channel.Send(chunk); err != nil {
				return
			}
			remaining -= int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			log.Debugf("filestream: read error: %v", readErr)
			return
		}
	}

	_ = channel.Close()
}
