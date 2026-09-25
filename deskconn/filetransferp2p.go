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

	"github.com/xconnio/deskconn/common"
)

// P2PChannelOpener is the subset of a connected P2P session needed merely
// to open a raw data channel on it. Depending on the interface rather than
// the concrete type also lets tests drive this code against a bare
// *webrtc.PeerConnection, with no WAMP handshake required.
type P2PChannelOpener interface {
	OpenChannel(label string, options *webrtc.DataChannelInit) (*webrtc.DataChannel, error)
}

// openP2PChannel opens a fresh, reliable, ordered raw data channel on
// sess's shared PeerConnection and waits for it to open.
func openP2PChannel(sess P2PChannelOpener, label string) (*webrtc.DataChannel, error) {
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
	case <-time.After(common.P2PRequestTimeout):
		_ = channel.Close()
		return nil, fmt.Errorf("timed out opening file-stream channel")
	}
}

func responseErr(resp common.FSResponse) error {
	if resp.Err == "" {
		return fmt.Errorf("remote operation failed")
	}
	return fmt.Errorf("%s", resp.Err) //nolint:err113
}

// p2pRequest opens a channel, performs the per-channel key exchange (see
// p2pClientKeyExchange), sends one encrypted request, and waits for the one
// encrypted response it expects back -- the pattern used by the list and
// init control ops, which carry no binary payload.
func p2pRequest(sess P2PChannelOpener, label string, req common.FSRequest) (*common.FSResponse, error) {
	channel, err := openP2PChannel(sess, label)
	if err != nil {
		return nil, err
	}
	defer channel.Close()

	closed, _ := common.WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		return nil, err
	}

	respCh := make(chan common.FSResponse, 1)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		kind, plaintext, err := common.DecryptEnvelope(msg.Data, receiveKey)
		if err != nil || kind != common.P2PMsgControl {
			return
		}
		var resp common.FSResponse
		if json.Unmarshal(plaintext, &resp) == nil {
			select {
			case respCh <- resp:
			default:
			}
		}
	})

	if err := common.SendEncryptedJSON(channel, req, sendKey); err != nil {
		return nil, err
	}

	resp, err := common.RecvPriority(respCh, closed, common.P2PRequestTimeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, responseErr(resp)
	}
	return &resp, nil
}

// p2pReadWorker opens one data channel on sess's shared PeerConnection and
// owns it for the lifetime of the goroutine running it, reusing it across
// every job pulled from jobs.
func p2pReadWorker(sess P2PChannelOpener, rootArg string, jobs <-chan transferChunk, localPath string,
	localIsDir bool, sourceRoot string, progress *common.TransferProgress) error {
	channel, err := openP2PChannel(sess, "filestream-read")
	if err != nil {
		return err
	}
	defer channel.Close()

	closed, _ := common.WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		return err
	}

	ackCh := make(chan common.FSResponse, 1)
	dataCh := make(chan []byte, 4)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		kind, plaintext, err := common.DecryptEnvelope(msg.Data, receiveKey)
		if err != nil {
			return
		}
		switch kind {
		case common.P2PMsgControl:
			var resp common.FSResponse
			if json.Unmarshal(plaintext, &resp) == nil {
				select {
				case ackCh <- resp:
				default:
				}
			}
		case common.P2PMsgData:
			select {
			case dataCh <- plaintext:
			case <-closed:
			}
		}
	})

	for chunk := range jobs {
		if err := p2pReadOneChunk(channel, closed, ackCh, dataCh, sendKey, rootArg, chunk, localPath, localIsDir,
			sourceRoot, progress); err != nil {
			return err
		}
	}
	return nil
}

func p2pReadOneChunk(channel *webrtc.DataChannel, closed <-chan struct{}, ackCh chan common.FSResponse,
	dataCh chan []byte, sendKey []byte, rootArg string, chunk transferChunk, localPath string, localIsDir bool,
	sourceRoot string,
	progress *common.TransferProgress) error {
	req := common.FSRequest{Op: common.FSOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset,
		Length: chunk.Length}
	if err := common.SendEncryptedJSON(channel, req, sendKey); err != nil {
		return err
	}

	ack, err := common.RecvPriority(ackCh, closed, common.P2PRequestTimeout)
	if err != nil {
		return err
	}
	if !ack.OK {
		return responseErr(ack)
	}

	dest := common.ResolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	var received int64
	for received < chunk.Length {
		data, err := common.RecvPriority(dataCh, closed, common.FileStreamRequestTimeout)
		if err != nil {
			return fmt.Errorf("receiving chunk data (%d/%d bytes): %w", received, chunk.Length, err)
		}
		n, err := ow.Write(data)
		received += int64(n)
		if progress != nil {
			progress.Add(int64(n))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// p2pWriteWorker is the upload counterpart to p2pReadWorker: it opens one
// data channel on sess's shared PeerConnection and owns it for the lifetime
// of the goroutine running it, reusing it across every job pulled from jobs.
func p2pWriteWorker(sess P2PChannelOpener, rootArg string, jobs <-chan transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) error {
	channel, err := openP2PChannel(sess, "filestream-write")
	if err != nil {
		return err
	}
	defer channel.Close()

	closed, sendReady := common.WebrtcBackpressure(channel)
	sendKey, receiveKey, err := p2pClientKeyExchange(channel, closed)
	if err != nil {
		return err
	}

	ackCh := make(chan common.FSResponse, 2)
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		kind, plaintext, err := common.DecryptEnvelope(msg.Data, receiveKey)
		if err != nil || kind != common.P2PMsgControl {
			return
		}
		var resp common.FSResponse
		if json.Unmarshal(plaintext, &resp) == nil {
			select {
			case ackCh <- resp:
			default:
			}
		}
	})

	for chunk := range jobs {
		if err := p2pWriteOneChunk(channel, closed, sendReady, ackCh, sendKey, rootArg, chunk, localBase,
			sourceIsDir, targetIsDirHint, progress); err != nil {
			return err
		}
	}
	return nil
}

func p2pWriteOneChunk(channel *webrtc.DataChannel, closed, sendReady <-chan struct{}, ackCh chan common.FSResponse,
	sendKey []byte, rootArg string, chunk transferChunk, localBase string, sourceIsDir, targetIsDirHint bool,
	progress *common.TransferProgress) error {
	req := common.FSRequest{
		Op: common.FSOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := common.SendEncryptedJSON(channel, req, sendKey); err != nil {
		return err
	}

	ack, err := common.RecvPriority(ackCh, closed, common.P2PRequestTimeout)
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
	buf := make([]byte, common.FileStreamChunkSize)
	for {
		n, readErr := section.Read(buf)
		if n > 0 {
			if err := common.SendEncryptedBytes(channel, closed, sendReady, buf[:n], sendKey); err != nil {
				return err
			}
			if progress != nil {
				progress.Add(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	final, err := common.RecvPriority(ackCh, closed, common.FileStreamRequestTimeout)
	if err != nil {
		return err
	}
	if !final.OK {
		return responseErr(final)
	}
	return nil
}

// DownloadFilesP2P downloads remotePath (a file, or if recursive a
// directory) from the device over parallel WebRTC data channels, all opened
// on sess's single shared PeerConnection (numWorkers <= 0 uses the
// default). Opening additional channels on an already-connected
// PeerConnection needs no new ICE/DTLS handshake, so every worker's channel
// comes up immediately once sess itself is connected.
func DownloadFilesP2P(sess P2PChannelOpener, remotePath, localPath string, recursive bool, numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) {
			return p2pRequest(sess, "filestream-list", req)
		},
		func(sourceRoot string, localIsDir bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return p2pReadWorker(sess, remotePath, jobs, localPath, localIsDir, sourceRoot, progress)
			}
		},
	)
}

// UploadFilesP2P uploads localPath (a file, or if recursive a directory) to
// the device over parallel WebRTC data channels, all opened on sess's
// single shared PeerConnection (numWorkers <= 0 uses the default). Opening
// additional channels on an already-connected PeerConnection needs no new
// ICE/DTLS handshake, so every worker's channel comes up immediately once
// sess itself is connected.
func UploadFilesP2P(sess P2PChannelOpener, localPath, remotePath string, recursive bool, numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) {
			return p2pRequest(sess, "filestream-init", req)
		},
		func(sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return p2pWriteWorker(sess, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint, progress)
			}
		},
	)
}
