package deskconn

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// relayErrorMsg is deskconn-router's error frame shape (see writeRelayError
// in deskconn-router's relay.go). The router can write one of these as the
// very first reply on a client-opened stream -- before the device (and so
// the key exchange) is ever reached -- e.g. when the realm is missing or
// the target device isn't currently connected. QuicClientKeyExchange checks
// for it so that case surfaces as the router's actual message instead of a
// confusing "invalid peer public key length: 0".
type relayErrorMsg struct {
	Error string `json:"error"`
}

// QuicClientKeyExchange performs the client side of the per-stream key
// exchange: send our ephemeral public key, receive the peer's, derive
// session keys. Must be called immediately after the routing frame and
// before anything else is sent on stream.
func QuicClientKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	publicKey, privateKey, err := common.CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}
	if err := common.WriteMsg(stream, common.KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	frame, err := common.ReadFrame(stream)
	if err != nil {
		return nil, nil, err
	}

	var relayErr relayErrorMsg
	if json.Unmarshal(frame, &relayErr) == nil && relayErr.Error != "" {
		return nil, nil, fmt.Errorf("%s", relayErr.Error) //nolint:err113
	}

	var peerKey common.KeyExchangeMsg
	if err := json.Unmarshal(frame, &peerKey); err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return common.ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}

func quicRequest(sess xconn.MultiplexedSession, realm string, req common.FSRequest) (*common.FSResponse, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	route := common.RoutingFrame{Realm: realm, Op: req.Op, Path: req.Path, Recursive: req.Recursive}
	if err := common.WriteMsg(stream, route); err != nil {
		return nil, err
	}

	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return nil, err
	}

	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return nil, err
	}

	var resp common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, responseErr(resp)
	}
	return &resp, nil
}

// quicReadWorker opens one stream on sess (the same connection shared by
// every parallel worker and the control call) and owns it for the lifetime
// of the goroutine running it, reusing it across every job pulled from
// jobs.
func quicReadWorker(sess xconn.MultiplexedSession, realm, rootArg string, jobs <-chan transferChunk, localPath string,
	localIsDir bool, sourceRoot string, progress *common.TransferProgress) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := common.WriteMsg(stream, common.RoutingFrame{Realm: realm, Op: common.FSOpRead, Path: rootArg}); err != nil {
		return err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return err
	}

	for chunk := range jobs {
		if err := quicReadOneChunk(stream, sendKey, receiveKey, rootArg, chunk, localPath, localIsDir, sourceRoot,
			progress); err != nil {
			return err
		}
	}
	return nil
}

func quicReadOneChunk(stream net.Conn, sendKey, receiveKey []byte, rootArg string, chunk transferChunk,
	localPath string, localIsDir bool, sourceRoot string, progress *common.TransferProgress) error {
	req := common.FSRequest{Op: common.FSOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset,
		Length: chunk.Length}
	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var resp common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return err
	}
	if !resp.OK {
		return responseErr(resp)
	}

	dest := common.ResolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	return common.CopyDecrypted(ow, stream, chunk.Length, receiveKey, progress)
}

// quicWriteWorker is the upload counterpart to quicReadWorker: it opens one
// stream on the shared sess and owns it for the lifetime of the goroutine
// running it, reusing it across every job pulled from jobs.
func quicWriteWorker(sess xconn.MultiplexedSession, realm, rootArg string, jobs <-chan transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := common.WriteMsg(stream, common.RoutingFrame{Realm: realm, Op: common.FSOpWrite, Path: rootArg}); err != nil {
		return err
	}
	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return err
	}

	for chunk := range jobs {
		if err := quicWriteOneChunk(stream, sendKey, receiveKey, rootArg, chunk, localBase, sourceIsDir,
			targetIsDirHint, progress); err != nil {
			return err
		}
	}
	return nil
}

func quicWriteOneChunk(stream net.Conn, sendKey, receiveKey []byte, rootArg string, chunk transferChunk,
	localBase string, sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) error {
	req := common.FSRequest{
		Op: common.FSOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := common.WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var ack common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &ack, receiveKey); err != nil {
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
	if err := common.CopyEncrypted(stream, section, chunk.Length, sendKey, progress); err != nil {
		return err
	}

	var final common.FSResponse
	if err := common.ReadEncryptedMsg(stream, &final, receiveKey); err != nil {
		return err
	}
	if !final.OK {
		return responseErr(final)
	}
	return nil
}

// DownloadFilesQUIC downloads remotePath (a file, or if recursive a
// directory) from the device over parallel QUIC streams, all opened on
// sess's single shared connection (numWorkers <= 0 uses the default). realm
// is needed by deskconn-router to route each stream (see RoutingFrame).
func DownloadFilesQUIC(sess xconn.MultiplexedSession, realm, remotePath, localPath string,
	recursive bool, numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceRoot string, localIsDir bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return quicReadWorker(sess, realm, remotePath, jobs, localPath, localIsDir, sourceRoot, progress)
			}
		},
	)
}

// UploadFilesQUIC uploads localPath (a file, or if recursive a directory)
// to the device over parallel QUIC streams, all opened on sess's single
// shared connection (numWorkers <= 0 uses the default). realm is needed by
// deskconn-router to route each stream (see RoutingFrame).
func UploadFilesQUIC(sess xconn.MultiplexedSession, realm, localPath, remotePath string,
	recursive bool, numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req common.FSRequest) (*common.FSResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceIsDir, targetIsDirHint bool, progress *common.TransferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return quicWriteWorker(sess, realm, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint,
					progress)
			}
		},
	)
}
