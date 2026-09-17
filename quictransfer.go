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

// maxMsgSize bounds readFrame's allocation. Control frames (fsRequest/
// fsResponse, possibly encrypted) are small; encrypted file-data chunks are
// bounded by encChunkSize plus a little AEAD overhead. Either way this is
// generous headroom rather than a tight fit.
const maxMsgSize = 1 << 20 // 1 MiB

// writeFrame/readFrame are the raw length-prefixed framing every message on
// a QUIC file-transfer stream uses, whether it carries plaintext JSON (the
// routingFrame and key exchange, which the router and the peer must be able
// to read) or an encrypted payload (everything after the key exchange).
func writeFrame(w io.Writer, data []byte) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data))) //nolint:gosec
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
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

func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, b)
}

func readMsg(r io.Reader, v any) error {
	buf, err := readFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// writeEncryptedMsg/readEncryptedMsg are writeMsg/readMsg's encrypted
// counterparts, used for every fsRequest/fsResponse once the per-stream key
// exchange has happened.
func writeEncryptedMsg(w io.Writer, v any, key []byte) error {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return err
	}
	return writeFrame(w, ciphertext)
}

func readEncryptedMsg(r io.Reader, v any, key []byte) error {
	ciphertext, err := readFrame(r)
	if err != nil {
		return err
	}
	plaintext, err := DecryptPayload(ciphertext, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(plaintext, v)
}

// AcceptQUICStreams runs an accept loop on a QUICSession, dispatching each server-initiated
// stream (e.g. opened by the cloud router on behalf of a CLI client) to HandleQUICStream.
func (d *Deskconn) AcceptQUICStreams(sess *xconn.QUICSession) {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return
		}
		SafeGo(func() { d.HandleQUICStream(nil, stream) })
	}
}

// routingFrame is the first message on every client-opened QUIC stream.
// deskconn-router's connBroker reads exactly these fields to route the
// stream to the right device, then bridges the rest of the stream through
// unmodified -- so it must be sent in the clear, before the key exchange
// (see keyExchangeMsg) and the encrypted real request that follow it.
type routingFrame struct {
	Realm     string `json:"realm"`
	Op        fsOp   `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// keyExchangeMsg carries one side's ephemeral X25519 public key. Everything
// after the exchange -- every fsRequest/fsResponse and all file data -- is
// ChaCha20-Poly1305 encrypted with the derived keys, since deskconn-router
// terminates QUIC/TLS at each hop rather than tunneling it through, and
// would otherwise see the plaintext it's relaying. Reused as-is for --mode
// p2p (see filestreamencryption.go).
type keyExchangeMsg struct {
	PublicKey []byte `json:"public_key"`
}

// relayErrorMsg is deskconn-router's error frame shape (see writeRelayError
// in deskconn-router's relay.go). The router can write one of these as the
// very first reply on a client-opened stream -- before the device (and so
// the key exchange) is ever reached -- e.g. when the realm is missing or
// the target device isn't currently connected. quicClientKeyExchange checks
// for it so that case surfaces as the router's actual message instead of a
// confusing "invalid peer public key length: 0".
type relayErrorMsg struct {
	Error string `json:"error"`
}

// quicClientKeyExchange performs the client side of the per-stream key
// exchange: send our ephemeral public key, receive the peer's, derive
// session keys. Must be called immediately after the routing frame and
// before anything else is sent on stream.
func quicClientKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return nil, nil, err
	}
	if err := writeMsg(stream, keyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}

	frame, err := readFrame(stream)
	if err != nil {
		return nil, nil, err
	}

	var relayErr relayErrorMsg
	if json.Unmarshal(frame, &relayErr) == nil && relayErr.Error != "" {
		return nil, nil, fmt.Errorf("%s", relayErr.Error) //nolint:err113
	}

	var peerKey keyExchangeMsg
	if err := json.Unmarshal(frame, &peerKey); err != nil {
		return nil, nil, err
	}
	if len(peerKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid peer public key length: %d", len(peerKey.PublicKey))
	}

	return ClientKeyExchangeKeys(privateKey, peerKey.PublicKey)
}

// quicServerKeyExchange is quicClientKeyExchange's server-side counterpart:
// receive the client's ephemeral public key, derive session keys, send back
// our own public key.
func quicServerKeyExchange(stream net.Conn) (sendKey, receiveKey []byte, err error) {
	var clientKey keyExchangeMsg
	if err := readMsg(stream, &clientKey); err != nil {
		return nil, nil, err
	}
	if len(clientKey.PublicKey) != 32 {
		return nil, nil, fmt.Errorf("invalid client public key length: %d", len(clientKey.PublicKey))
	}

	publicKey, sendKey, receiveKey, err := ServerKeyExchange(clientKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if err := writeMsg(stream, keyExchangeMsg{PublicKey: publicKey}); err != nil {
		return nil, nil, err
	}
	return sendKey, receiveKey, nil
}

// HandleQUICStream serves a single QUIC stream. The leading routingFrame's
// Op says which protocol the rest of the stream speaks: fsOpShell goes to
// handleQUICShellStream, fsOpPortForward/fsOpPortReverse go to portstream.go's
// handlers, fsOpAgentForward goes to agentforwardstream.go's handler; list/init
// are one-shot file-transfer requests that reply and return; read/write go to
// quicServeSession, which keeps the stream open across many chunk requests
// from the same worker -- reopening a stream per chunk was measured to badly
// limit throughput on real (non-loopback) links.
func (d *Deskconn) HandleQUICStream(_ xconn.BaseSession, stream net.Conn) {
	defer stream.Close()

	_ = stream.SetReadDeadline(time.Now().Add(fileStreamSessionIdleTimeout))
	var route routingFrame
	if err := readMsg(stream, &route); err != nil {
		return
	}

	switch route.Op {
	case fsOpShell:
		d.handleQUICShellStream(stream)
		return
	case fsOpPortForward:
		d.handleQUICPortForwardStream(stream)
		return
	case fsOpPortReverse:
		d.handleQUICPortReverseStream(stream)
		return
	case fsOpAgentForward:
		d.handleQUICAgentForwardStream(stream)
		return
	}

	sendKey, receiveKey, err := quicServerKeyExchange(stream)
	if err != nil {
		return
	}

	var req fsRequest
	if err := readEncryptedMsg(stream, &req, receiveKey); err != nil {
		return
	}

	switch req.Op {
	case fsOpList:
		quicServeList(stream, req, sendKey)
	case fsOpInit:
		quicServeInit(stream, req, sendKey)
	case fsOpRead, fsOpWrite:
		quicServeSession(stream, req, sendKey, receiveKey)
	}
}

// quicServeSession serves req and then any further read/write requests the
// client sends on the same stream, one at a time, until the client closes
// its side (a normal end of session, surfaced here as a read error) or a
// transport failure occurs.
func quicServeSession(stream net.Conn, req fsRequest, sendKey, receiveKey []byte) {
	for {
		var err error
		switch req.Op {
		case fsOpRead:
			err = quicServeReadOnce(stream, req, sendKey)
		case fsOpWrite:
			err = quicServeWriteOnce(stream, req, sendKey, receiveKey)
		default:
			return
		}
		if err != nil {
			return
		}

		_ = stream.SetReadDeadline(time.Now().Add(fileStreamSessionIdleTimeout))
		if err := readEncryptedMsg(stream, &req, receiveKey); err != nil {
			return
		}
	}
}

func quicServeList(stream net.Conn, req fsRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
		return
	}
	entries, err := buildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = writeEncryptedMsg(stream, fsResponse{OK: true, Entries: entries}, sendKey)
}

// quicServeReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// fsResponse.Err and treated as handled.
func quicServeReadOnce(stream net.Conn, req fsRequest, sendKey []byte) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		return writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		return writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := writeEncryptedMsg(stream, fsResponse{OK: true}, sendKey); err != nil {
		return err
	}

	section := io.NewSectionReader(f, req.Offset, req.Length)
	return copyEncrypted(stream, section, req.Length, sendKey, nil)
}

func quicServeInit(stream net.Conn, req fsRequest, sendKey []byte) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
		return
	}
	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := materializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
		return
	}
	_ = writeEncryptedMsg(stream, fsResponse{OK: true}, sendKey)
}

// quicServeWriteOnce serves one byte-range write. Same error-return
// convention as quicServeReadOnce.
func quicServeWriteOnce(stream net.Conn, req fsRequest, sendKey, receiveKey []byte) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		return writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
	}
	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// See serveWebRTCWriteOnce for why passing req.RelPath as sourceRoot is safe.
	dest := resolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return writeEncryptedMsg(stream, fsResponse{Err: err.Error()}, sendKey)
	}
	defer f.Close()

	if err := writeEncryptedMsg(stream, fsResponse{OK: true}, sendKey); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	if err := copyDecrypted(ow, stream, req.Length, receiveKey, nil); err != nil {
		return err
	}
	return writeEncryptedMsg(stream, fsResponse{OK: true}, sendKey)
}

// encChunkSize is the plaintext size of each encrypted file-data message,
// set near the maxMsgSize ceiling QUIC streams have no hard transport limit
// requiring anything smaller.
const encChunkSize = 1000000

// copyEncrypted reads exactly n plaintext bytes from src, encrypting and
// framing each chunk as it goes, reporting progress as real (plaintext)
// bytes processed.
func copyEncrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *transferProgress) error {
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
			if err := writeFrame(dst, ciphertext); err != nil {
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

// copyDecrypted is copyEncrypted's counterpart: reads encrypted chunks from
// src until exactly n plaintext bytes have been written to dst.
func copyDecrypted(dst io.Writer, src io.Reader, n int64, key []byte, progress *transferProgress) error {
	var written int64
	for written < n {
		ciphertext, err := readFrame(src)
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

func quicRequest(sess xconn.MultiplexedSession, realm string, req fsRequest) (*fsResponse, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	route := routingFrame{Realm: realm, Op: req.Op, Path: req.Path, Recursive: req.Recursive}
	if err := writeMsg(stream, route); err != nil {
		return nil, err
	}

	sendKey, receiveKey, err := quicClientKeyExchange(stream)
	if err != nil {
		return nil, err
	}

	if err := writeEncryptedMsg(stream, req, sendKey); err != nil {
		return nil, err
	}

	var resp fsResponse
	if err := readEncryptedMsg(stream, &resp, receiveKey); err != nil {
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

	if err := writeMsg(stream, routingFrame{Realm: realm, Op: fsOpRead, Path: rootArg}); err != nil {
		return err
	}
	sendKey, receiveKey, err := quicClientKeyExchange(stream)
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
	req := fsRequest{Op: fsOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length}
	if err := writeEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var resp fsResponse
	if err := readEncryptedMsg(stream, &resp, receiveKey); err != nil {
		return err
	}
	if !resp.OK {
		return responseErr(resp)
	}

	dest := resolveDestPath(localPath, localIsDir, sourceRoot, chunk.RelPath)
	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close()

	ow := io.NewOffsetWriter(f, chunk.Offset)
	return copyDecrypted(ow, stream, chunk.Length, receiveKey, progress)
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

	if err := writeMsg(stream, routingFrame{Realm: realm, Op: fsOpWrite, Path: rootArg}); err != nil {
		return err
	}
	sendKey, receiveKey, err := quicClientKeyExchange(stream)
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
	req := fsRequest{
		Op: fsOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := writeEncryptedMsg(stream, req, sendKey); err != nil {
		return err
	}

	var ack fsResponse
	if err := readEncryptedMsg(stream, &ack, receiveKey); err != nil {
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
	if err := copyEncrypted(stream, section, chunk.Length, sendKey, progress); err != nil {
		return err
	}

	var final fsResponse
	if err := readEncryptedMsg(stream, &final, receiveKey); err != nil {
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
// is needed by deskconn-router to route each stream (see routingFrame).
func DownloadFilesQUIC(sess xconn.MultiplexedSession, realm, remotePath, localPath string,
	recursive bool, numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return quicRequest(sess, realm, req) },
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
// deskconn-router to route each stream (see routingFrame).
func UploadFilesQUIC(sess xconn.MultiplexedSession, realm, localPath, remotePath string,
	recursive bool, numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceIsDir, targetIsDirHint bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				return quicWriteWorker(sess, realm, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint,
					progress)
			}
		},
	)
}
