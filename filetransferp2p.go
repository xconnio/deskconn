package deskconn

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// p2pRequestTimeout bounds how long a client waits for a channel to open or
// for the remote side's response to a control request (list/init) or the
// first ack on a read/write chunk request.
const p2pRequestTimeout = 15 * time.Second

// p2pChannelOpener is the subset of a connected P2P session needed merely
// to open a raw data channel on it. Depending on the interface rather than
// the concrete type also lets tests drive this code against a bare
// *webrtc.PeerConnection, with no WAMP handshake required.
type p2pChannelOpener interface {
	OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error)
}

// P2PConnection is a connected, closeable P2P session -- everything one
// parallel worker needs from its own independent connection.
// *xconnwebrtc.WebRTCSession satisfies this.
type P2PConnection interface {
	p2pChannelOpener
	Close() error
}

// P2PConnector opens one fresh, independent P2P connection (its own
// ICE/DTLS/SCTP handshake) on demand. DownloadFilesP2P/UploadFilesP2P call it
// once per parallel worker, so each worker gets its own PeerConnection
// rather than a data channel sharing one: data channels opened on the same
// PeerConnection all ride that PeerConnection's single SCTP association,
// which has ONE shared congestion-control state for the whole connection.
// Any backoff on that one association throttles every "parallel" channel
// on it at once -- unlike genuinely independent connections (the way
// download managers and browsers get their speedup from parallel
// transfers), which each get their own congestion window.
type P2PConnector func() (P2PConnection, error)

// openP2PChannel opens a fresh, reliable, ordered raw data channel on
// sess's shared PeerConnection and waits for it to open.
func openP2PChannel(sess p2pChannelOpener, label string) (*webrtc.DataChannel, error) {
	channel, err := sess.OpenChannel(label, nil)
	if err != nil {
		return nil, err
	}

	closedCh := make(chan struct{})
	var closedOnce sync.Once
	signalClosed := func() { closedOnce.Do(func() { close(closedCh) }) }
	channel.OnClose(signalClosed)
	channel.OnError(func(error) { signalClosed() })

	openCh := make(chan struct{})
	channel.OnOpen(func() { close(openCh) })

	select {
	case <-openCh:
		return channel, nil
	case <-closedCh:
		return nil, fmt.Errorf("remote closed the file-stream channel before it opened")
	case <-time.After(p2pRequestTimeout):
		_ = channel.Close()
		return nil, fmt.Errorf("timed out opening file-stream channel")
	}
}

func responseErr(resp fsResponse) error {
	if resp.Err == "" {
		return fmt.Errorf("remote operation failed")
	}
	return fmt.Errorf("%s", resp.Err) //nolint:err113
}

// p2pRequest opens a channel, sends one JSON request, and waits for the one
// JSON response it expects back -- the pattern used by the list and init
// control ops, which carry no binary payload.
func p2pRequest(sess p2pChannelOpener, label string, req fsRequest) (*fsResponse, error) {
	channel, err := openP2PChannel(sess, label)
	if err != nil {
		return nil, err
	}
	defer channel.Close()

	closed, _ := webrtcBackpressure(channel)
	respCh := make(chan fsResponse, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var resp fsResponse
		if json.Unmarshal(msg.Data, &resp) == nil {
			select {
			case respCh <- resp:
			default:
			}
		}
	})

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := channel.SendText(string(payload)); err != nil {
		return nil, err
	}

	resp, err := recvPriority(respCh, closed, p2pRequestTimeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, responseErr(resp)
	}
	return &resp, nil
}

// p2pReadWorker owns one raw data channel for the lifetime of the goroutine
// running it, reusing it across every job pulled from jobs -- opening a
// fresh channel per chunk was measured to badly limit throughput on real
// (non-loopback) links, since the channel never reaches steady-state flow
// control before it's torn down again.
func p2pReadWorker(sess p2pChannelOpener, rootArg string, jobs <-chan transferChunk, localPath string,
	localIsDir bool, sourceRoot string, progress *transferProgress) error {
	channel, err := openP2PChannel(sess, "filestream-read")
	if err != nil {
		return err
	}
	defer channel.Close()

	closed, _ := webrtcBackpressure(channel)
	ackCh := make(chan fsResponse, 1)
	dataCh := make(chan []byte, 4)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			var resp fsResponse
			if json.Unmarshal(msg.Data, &resp) == nil {
				select {
				case ackCh <- resp:
				default:
				}
			}
			return
		}
		select {
		case dataCh <- msg.Data:
		case <-closed:
		}
	})

	for chunk := range jobs {
		if err := p2pReadOneChunk(channel, closed, ackCh, dataCh, rootArg, chunk, localPath, localIsDir,
			sourceRoot, progress); err != nil {
			return err
		}
	}
	return nil
}

func p2pReadOneChunk(channel *webrtc.DataChannel, closed <-chan struct{}, ackCh chan fsResponse, dataCh chan []byte,
	rootArg string, chunk transferChunk, localPath string, localIsDir bool, sourceRoot string,
	progress *transferProgress) error {
	req := fsRequest{Op: fsOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := channel.SendText(string(payload)); err != nil {
		return err
	}

	ack, err := recvPriority(ackCh, closed, p2pRequestTimeout)
	if err != nil {
		return err
	}
	if !ack.OK {
		return responseErr(ack)
	}

	dest := resolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	var received int64
	for received < chunk.Length {
		data, err := recvPriority(dataCh, closed, fileStreamRequestTimeout)
		if err != nil {
			return fmt.Errorf("receiving chunk data (%d/%d bytes): %w", received, chunk.Length, err)
		}
		n, err := ow.Write(data)
		received += int64(n)
		if progress != nil {
			progress.add(int64(n))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// p2pWriteWorker is the upload counterpart to p2pReadWorker: it owns one raw
// data channel for the lifetime of the goroutine running it, reusing it
// across every job pulled from jobs.
func p2pWriteWorker(sess p2pChannelOpener, rootArg string, jobs <-chan transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *transferProgress) error {
	channel, err := openP2PChannel(sess, "filestream-write")
	if err != nil {
		return err
	}
	defer channel.Close()

	closed, sendReady := webrtcBackpressure(channel)
	ackCh := make(chan fsResponse, 2)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			return
		}
		var resp fsResponse
		if json.Unmarshal(msg.Data, &resp) == nil {
			select {
			case ackCh <- resp:
			default:
			}
		}
	})

	for chunk := range jobs {
		if err := p2pWriteOneChunk(channel, closed, sendReady, ackCh, rootArg, chunk, localBase,
			sourceIsDir, targetIsDirHint, progress); err != nil {
			return err
		}
	}
	return nil
}

func p2pWriteOneChunk(channel *webrtc.DataChannel, closed, sendReady <-chan struct{}, ackCh chan fsResponse,
	rootArg string, chunk transferChunk, localBase string, sourceIsDir, targetIsDirHint bool,
	progress *transferProgress) error {
	req := fsRequest{
		Op: fsOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := channel.SendText(string(payload)); err != nil {
		return err
	}

	ack, err := recvPriority(ackCh, closed, p2pRequestTimeout)
	if err != nil {
		return err
	}
	if !ack.OK {
		return responseErr(ack)
	}

	absLocal := filepath.Join(localBase, filepath.FromSlash(chunk.RelPath))
	f, err := os.Open(absLocal) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	section := io.NewSectionReader(f, chunk.Offset, chunk.Length)
	buf := make([]byte, fileStreamChunkSize)
	for {
		n, readErr := section.Read(buf)
		if n > 0 {
			if err := sendWebRTCBytes(channel, closed, sendReady, buf[:n]); err != nil {
				return err
			}
			if progress != nil {
				progress.add(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	final, err := recvPriority(ackCh, closed, fileStreamRequestTimeout)
	if err != nil {
		return err
	}
	if !final.OK {
		return responseErr(final)
	}
	return nil
}

// DownloadFilesP2P downloads remotePath (a file, or if recursive a directory)
// from the device over parallel raw WebRTC connections -- no WAMP call
// carries any of the file's bytes. sess (already connected) serves the
// one-shot list control request; connect opens one independent connection
// per parallel worker -- see P2PConnector's doc comment for why that
// matters for throughput. numWorkers <= 0 uses the default
// (parallelStreamWorkers). Thin wrapper around the transport-agnostic
// downloadFiles orchestrator, supplying the WebRTC-specific list call and
// per-worker connection.
func DownloadFilesP2P(sess p2pChannelOpener, connect P2PConnector, remotePath, localPath string, recursive bool,
	numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return p2pRequest(sess, "filestream-list", req) },
		func(sourceRoot string, localIsDir bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				workerConn, err := connect()
				if err != nil {
					return err
				}
				defer func() { _ = workerConn.Close() }()
				return p2pReadWorker(workerConn, remotePath, jobs, localPath, localIsDir, sourceRoot, progress)
			}
		},
	)
}

// UploadFilesP2P uploads localPath (a file, or if recursive a directory) to
// the device over parallel raw WebRTC connections -- no WAMP call carries
// any of the file's bytes. sess (already connected) serves the one-shot
// init control request; connect opens one independent connection per
// parallel worker -- see P2PConnector's doc comment for why that matters
// for throughput. numWorkers <= 0 uses the default (parallelStreamWorkers).
// Thin wrapper around the transport-agnostic uploadFiles orchestrator,
// supplying the WebRTC-specific init call and per-worker connection.
func UploadFilesP2P(sess p2pChannelOpener, connect P2PConnector, localPath, remotePath string, recursive bool,
	numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return p2pRequest(sess, "filestream-init", req) },
		func(sourceIsDir, targetIsDirHint bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				workerConn, err := connect()
				if err != nil {
					return err
				}
				defer func() { _ = workerConn.Close() }()
				return p2pWriteWorker(workerConn, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint, progress)
			}
		},
	)
}
