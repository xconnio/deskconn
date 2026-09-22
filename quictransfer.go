package deskconn

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/xconnio/xconn-go"
)

// maxMsgSize bounds ReadFrame's allocation. Control frames (FSRequest/
// FSResponse, possibly encrypted) are small; encrypted file-data chunks are
// bounded by encChunkSize plus a little AEAD overhead. Either way this is
// generous headroom rather than a tight fit.
const maxMsgSize = 1 << 20 // 1 MiB

// WriteFrame/ReadFrame are the raw length-prefixed framing every message on
// a QUIC file-transfer stream uses, whether it carries plaintext JSON (the
// RoutingFrame and key exchange, which the router and the peer must be able
// to read) or an encrypted payload (everything after the key exchange).
func WriteFrame(w io.Writer, data []byte) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data))) //nolint:gosec
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func ReadFrame(r io.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n > maxMsgSize {
		return nil, fmt.Errorf("message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, b)
}

func ReadMsg(r io.Reader, v any) error {
	buf, err := ReadFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// WriteEncryptedMsg/ReadEncryptedMsg are WriteMsg/ReadMsg's encrypted
// counterparts, used for every FSRequest/FSResponse once the per-stream key
// exchange has happened.
func WriteEncryptedMsg(w io.Writer, v any, key []byte) error {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return err
	}
	return WriteFrame(w, ciphertext)
}

func ReadEncryptedMsg(r io.Reader, v any, key []byte) error {
	ciphertext, err := ReadFrame(r)
	if err != nil {
		return err
	}
	plaintext, err := DecryptPayload(ciphertext, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, v)
}

// streamOpReadTimeout bounds how long ReadStreamOp waits for the leading
// RoutingFrame before giving up on a stalled/malicious stream.
const streamOpReadTimeout = 30 * time.Second

// RoutingFrame is the first message on every client-opened QUIC stream.
// deskconn-router's connBroker reads exactly these fields to route the
// stream to the right device, then bridges the rest of the stream through
// unmodified -- so it must be sent in the clear, before the key exchange
// (see KeyExchangeMsg) and the encrypted real request that follow it.
type RoutingFrame struct {
	Realm     string `json:"realm"`
	Op        FSOp   `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// KeyExchangeMsg carries one side's ephemeral X25519 public key. Everything
// after the exchange -- every FSRequest/FSResponse and all file data -- is
// ChaCha20-Poly1305 encrypted with the derived keys, since deskconn-router
// terminates QUIC/TLS at each hop rather than tunneling it through, and
// would otherwise see the plaintext it's relaying. Reused as-is for --mode
// p2p (see filestreamencryption.go).
type KeyExchangeMsg struct {
	PublicKey []byte `json:"public_key"`
}

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
	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}
	if err := WriteMsg(stream, KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	frame, err := ReadFrame(stream)
	if err != nil {
		return nil, nil, err
	}

	var relayErr relayErrorMsg
	if json.Unmarshal(frame, &relayErr) == nil && relayErr.Error != "" {
		return nil, nil, fmt.Errorf("%s", relayErr.Error) //nolint:err113
	}

	var peerKey KeyExchangeMsg
	if err := json.Unmarshal(frame, &peerKey); err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}

// ReadStreamOp reads the leading RoutingFrame off a freshly accepted QUIC
// stream and returns which feature the rest of the stream belongs to,
// without needing to understand any of that feature's own payload -- this
// is exactly what xlink needs to classify an inbound stream and relay
// it to the right local backend. The routing frame is consumed; whatever
// comes after it on stream is untouched and ready for the backend to read.
func ReadStreamOp(stream net.Conn) (FSOp, error) {
	_ = stream.SetReadDeadline(time.Now().Add(streamOpReadTimeout))
	var route RoutingFrame
	if err := ReadMsg(stream, &route); err != nil {
		return "", err
	}
	return route.Op, nil
}

// DispatchQUICOp resumes handling a QUIC stream that deskconnd has already
// classified and relayed locally: op is the routingFrame.Op deskconnd read
// before relaying (see ReadStreamOp), and stream is everything
// after that routing frame, untouched -- exactly what deskconnd's old,
// single-process HandleQUICStream saw right after its own read of it.
func (d *Deskconn) DispatchQUICOp(op FSOp, stream net.Conn) {
	defer stream.Close()

	switch op {
	case FSOpShell:
		d.handleQUICShellStream(stream)
		return
	case FSOpPortForward:
		d.handleQUICPortForwardStream(stream)
		return
	case FSOpPortReverse:
		d.handleQUICPortReverseStream(stream)
		return
	case FSOpAgentForward:
		d.handleQUICAgentForwardStream(stream)
		return
	case FSOpLogs:
		d.handleQUICLogsStream(stream)
		return
	}

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	var req FSRequest
	if err := ReadEncryptedMsg(stream, &req, receiveKey); err != nil {
		return
	}

	switch req.Op {
	case FSOpList:
		quicServeList(stream, req, sendKey)
	case FSOpInit:
		quicServeInit(stream, req, sendKey)
	case FSOpRead, FSOpWrite:
		quicServeSession(stream, req, sendKey, receiveKey)
	}
}

// quicServerKeyExchange is the client's quicClientKeyExchange's server-side
// counterpart: receive the client's ephemeral public key, derive session
// keys, send back our own public key.
func quicServerKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	var clientKey KeyExchangeMsg
	if err := ReadMsg(stream, &clientKey); err != nil {
		return nil, nil, err
	}
	if len(clientKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid client public key length: %d", len(clientKey.PublicKey))
	}

	publicKey, sendKey, receiveKey, err := ServerKeyExchange(clientKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if err := WriteMsg(stream, KeyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}
	return sendKey, receiveKey, nil
}

// quicServeSession serves req and then any further read/write requests the
// client sends on the same stream, one at a time, until the client closes
// its side (a normal end of session, surfaced here as a read error) or a
// transport failure occurs.
func quicServeSession(stream net.Conn, req FSRequest, sendKey, receiveKey []byte) {
	for {
		var err error
		switch req.Op {
		case FSOpRead:
			err = quicServeReadOnce(stream, req, sendKey)
		case FSOpWrite:
			err = quicServeWriteOnce(stream, req, sendKey, receiveKey)
		default:
			return
		}
		if err != nil {
			return
		}

		_ = stream.SetReadDeadline(time.Now().Add(FileStreamSessionIdleTimeout))
		if err := ReadEncryptedMsg(stream, &req, receiveKey); err != nil {
			return
		}
	}
}

func quicServeList(stream net.Conn, req FSRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
		return
	}
	entries, err := BuildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = WriteEncryptedMsg(stream, FSResponse{OK: true, Entries: entries}, sendKey)
}

// quicServeReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// FSResponse.Err and treated as handled.
func quicServeReadOnce(stream net.Conn, req FSRequest, sendKey []byte) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		return WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		return WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := WriteEncryptedMsg(stream, FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	section := io.NewSectionReader(f, req.Offset, req.Length)
	return CopyEncrypted(stream, section, req.Length, sendKey, nil)
}

func quicServeInit(stream net.Conn, req FSRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
		return
	}
	rootIsDir := IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := MaterializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = WriteEncryptedMsg(stream, FSResponse{OK: true}, sendKey)
}

// quicServeWriteOnce serves one byte-range write. Same error-return
// convention as quicServeReadOnce.
func quicServeWriteOnce(stream net.Conn, req FSRequest, sendKey, receiveKey []byte) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		return WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
	}
	rootIsDir := IsRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// See serveWebRTCWriteOnce for why passing req.RelPath as sourceRoot is safe.
	dest := ResolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return WriteEncryptedMsg(stream, FSResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := WriteEncryptedMsg(stream, FSResponse{OK: true}, sendKey); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	if err := CopyDecrypted(ow, stream, req.Length, receiveKey, nil); err != nil {
		return err
	}
	return WriteEncryptedMsg(stream, FSResponse{OK: true}, sendKey)
}

// encChunkSize is the plaintext size of each encrypted file-data message,
// set near the maxMsgSize ceiling QUIC streams have no hard transport limit
// requiring anything smaller.
const encChunkSize = 1000000

// CopyEncrypted reads exactly n plaintext bytes from src, encrypting and
// framing each chunk as it goes, reporting progress as real (plaintext)
// bytes processed.
func CopyEncrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *transferProgress) error {
	buf := make([]byte, encChunkSize)
	var written int64
	for written < n {
		toRead := int64(len(buf))
		if remaining := n - written; remaining < toRead {
			toRead = remaining
		}
		rn, rerr := io.ReadFull(src, buf[:toRead])
		if rn > 0 {
			ciphertext, err := EncryptPayload(buf[:rn], key)
			if err != nil {
				return err
			}
			if err := WriteFrame(dst, ciphertext); err != nil {
				return err
			}
			written += int64(rn)
			if progress != nil {
				progress.add(int64(rn))
			}
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// CopyDecrypted is CopyEncrypted's counterpart: reads encrypted chunks from
// src until exactly n plaintext bytes have been written to dst.
func CopyDecrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *transferProgress) error {
	var written int64
	for written < n {
		ciphertext, err := ReadFrame(src)
		if err != nil {
			return err
		}
		plaintext, err := DecryptPayload(ciphertext, key)
		if err != nil {
			return err
		}
		wn, werr := dst.Write(plaintext)
		written += int64(wn)
		if progress != nil {
			progress.add(int64(wn))
		}
		if werr != nil {
			return werr
		}
	}
	return nil
}

func quicRequest(sess xconn.MultiplexedSession, realm string, req FSRequest) (*FSResponse, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	route := RoutingFrame{Realm: realm, Op: req.Op, Path: req.Path, Recursive: req.Recursive}
	if err := WriteMsg(stream, route); err != nil {
		return nil, err
	}

	sendKey, receiveKey, err := QuicClientKeyExchange(stream)
	if err != nil {
		return nil, err
	}

	if err := WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return nil, err
	}

	var resp FSResponse
	if err := ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
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
	localIsDir bool, sourceRoot string, progress *transferProgress) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpRead, Path: rootArg}); err != nil {
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
	localPath string, localIsDir bool, sourceRoot string, progress *transferProgress) error {
	req := FSRequest{Op: FSOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length}
	if err := WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var resp FSResponse
	if err := ReadEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return err
	}
	if !resp.OK {
		return responseErr(resp)
	}

	dest := ResolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	return CopyDecrypted(ow, stream, chunk.Length, receiveKey, progress)
}

// quicWriteWorker is the upload counterpart to quicReadWorker: it opens one
// stream on the shared sess and owns it for the lifetime of the goroutine
// running it, reusing it across every job pulled from jobs.
func quicWriteWorker(sess xconn.MultiplexedSession, realm, rootArg string, jobs <-chan transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *transferProgress) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := WriteMsg(stream, RoutingFrame{Realm: realm, Op: FSOpWrite, Path: rootArg}); err != nil {
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
	localBase string, sourceIsDir, targetIsDirHint bool, progress *transferProgress) error {
	req := FSRequest{
		Op: FSOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := WriteEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var ack FSResponse
	if err := ReadEncryptedMsg(stream, &ack, receiveKey); err != nil {
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
	if err := CopyEncrypted(stream, section, chunk.Length, sendKey, progress); err != nil {
		return err
	}

	var final FSResponse
	if err := ReadEncryptedMsg(stream, &final, receiveKey); err != nil {
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
		func(req FSRequest) (*FSResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceRoot string, localIsDir bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
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
		func(req FSRequest) (*FSResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceIsDir, targetIsDirHint bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return quicWriteWorker(sess, realm, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint,
					progress)
			}
		},
	)
}
