package deskconn

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/xconnio/xconn-go"
)

// maxMsgSize bounds readMsg's allocation. Messages are small JSON control
// frames (fsRequest/fsResponse), never file content, so this is generous
// headroom rather than a tight fit.
const maxMsgSize = 1 << 20 // 1 MiB

func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(b))) //nolint:gosec
	if _, err = w.Write(length[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readMsg(r io.Reader, v any) error {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n > maxMsgSize {
		return fmt.Errorf("message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
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

// routingFrame is the exact shape deskconn-router's connBroker expects as
// the first message on any client-opened raw QUIC stream, so it can look up
// the target device's connection by realm and relay the stream to it. It
// re-encodes and forwards only these four fields to the device -- anything
// else on the real request (RelPath, Offset, Length, Entries, ...) would be
// silently dropped if sent as that first message. So every freshly opened
// stream sends this frame first (see quicRequest/quicReadWorker/
// quicWriteWorker), then the real fsRequest as a second message once the
// router has bridged the stream straight through to the device -- which is
// why HandleQUICStream always discards one leading message before reading
// the real request.
type routingFrame struct {
	Realm     string `json:"realm"`
	Op        fsOp   `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// HandleQUICStream handles a single raw QUIC stream carrying one or more
// file-transfer requests -- list/init are one-shot control requests, sent
// once per whole transfer; read/write are handed to quicServeSession, which
// keeps the stream open across many chunk requests from the same worker.
// Reopening a stream per chunk was measured to badly limit throughput on
// real (non-loopback) links, since the stream never reaches steady-state
// flow control before it's torn down again. This mirrors the WebRTC
// transport in filestreamchannel.go, sharing the fsRequest/fsResponse
// protocol and the parallel chunk-worker logic in filetransfer.go.
func (d *Deskconn) HandleQUICStream(_ xconn.BaseSession, stream net.Conn) {
	defer stream.Close()

	// Leading routingFrame, present on every stream once it's reached here
	// via the router -- see routingFrame's doc comment. Discarded.
	var discard fsRequest
	if err := readMsg(stream, &discard); err != nil {
		return
	}

	var req fsRequest
	if err := readMsg(stream, &req); err != nil {
		return
	}

	switch req.Op {
	case fsOpList:
		quicServeList(stream, req)
	case fsOpInit:
		quicServeInit(stream, req)
	case fsOpRead, fsOpWrite:
		quicServeSession(stream, req)
	}
}

// quicServeSession serves req and then any further read/write requests the
// client sends on the same stream, one at a time, until the client closes
// its side (a normal end of session, surfaced here as a read error) or a
// transport failure occurs.
func quicServeSession(stream net.Conn, req fsRequest) {
	for {
		var err error
		switch req.Op {
		case fsOpRead:
			err = quicServeReadOnce(stream, req)
		case fsOpWrite:
			err = quicServeWriteOnce(stream, req)
		default:
			return
		}
		if err != nil {
			return
		}

		if err := readMsg(stream, &req); err != nil {
			return
		}
	}
}

func quicServeList(stream net.Conn, req fsRequest) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = writeMsg(stream, fsResponse{Err: err.Error()})
		return
	}
	entries, err := buildManifest(resolvedRoot, req.Recursive)
	if err != nil {
		_ = writeMsg(stream, fsResponse{Err: err.Error()})
		return
	}
	_ = writeMsg(stream, fsResponse{OK: true, Entries: entries})
}

// quicServeReadOnce serves one byte-range read. It only returns a non-nil
// error for a transport failure that should end the session; an
// application-level failure (bad path, etc.) is reported to the client via
// fsResponse.Err and treated as handled.
func quicServeReadOnce(stream net.Conn, req fsRequest) error {
	_, basePath, err := remoteRootAndBase(req.Path)
	if err != nil {
		return writeMsg(stream, fsResponse{Err: err.Error()})
	}
	absPath := filepath.Join(basePath, filepath.FromSlash(req.RelPath))

	f, err := os.Open(absPath) //nolint:gosec
	if err != nil {
		return writeMsg(stream, fsResponse{Err: err.Error()})
	}
	defer f.Close()

	if err := writeMsg(stream, fsResponse{OK: true}); err != nil {
		return err
	}

	section := io.NewSectionReader(f, req.Offset, req.Length)
	_, err = io.Copy(stream, section)
	return err
}

func quicServeInit(stream net.Conn, req fsRequest) {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		_ = writeMsg(stream, fsResponse{Err: err.Error()})
		return
	}
	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	if err := materializeTargets(req.Entries, resolvedRoot, rootIsDir); err != nil {
		_ = writeMsg(stream, fsResponse{Err: err.Error()})
		return
	}
	_ = writeMsg(stream, fsResponse{OK: true})
}

// quicServeWriteOnce serves one byte-range write. Same error-return
// convention as quicServeReadOnce.
func quicServeWriteOnce(stream net.Conn, req fsRequest) error {
	resolvedRoot, _, err := remoteRootAndBase(req.Path)
	if err != nil {
		return writeMsg(stream, fsResponse{Err: err.Error()})
	}
	rootIsDir := isRootDir(resolvedRoot, req.SourceIsDir, req.TargetIsDirHint)
	// See serveWebRTCWriteOnce for why passing req.RelPath as sourceRoot is safe.
	dest := resolveDestPath(resolvedRoot, rootIsDir, req.RelPath, req.RelPath)

	f, err := os.OpenFile(dest, os.O_WRONLY, 0) //nolint:gosec
	if err != nil {
		return writeMsg(stream, fsResponse{Err: err.Error()})
	}
	defer f.Close()

	if err := writeMsg(stream, fsResponse{OK: true}); err != nil {
		return err
	}

	ow := io.NewOffsetWriter(f, req.Offset)
	if _, err := io.CopyN(ow, stream, req.Length); err != nil {
		return err
	}
	return writeMsg(stream, fsResponse{OK: true})
}

// copyWithProgress copies exactly n bytes from src to dst, reporting each
// write to progress as it happens (unlike io.CopyN, which gives no
// incremental hook).
func copyWithProgress(dst io.Writer, src io.Reader, n int64, progress *transferProgress) error {
	buf := make([]byte, fileChunkSize)
	var written int64
	for written < n {
		toRead := int64(len(buf))
		if remaining := n - written; remaining < toRead {
			toRead = remaining
		}
		rn, rerr := src.Read(buf[:toRead])
		if rn > 0 {
			wn, werr := dst.Write(buf[:rn])
			written += int64(wn)
			if progress != nil {
				progress.add(int64(wn))
			}
			if werr != nil {
				return werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF { //nolint:errorlint
				break
			}
			return rerr
		}
	}
	return nil
}

// QUICStreamCloser is what DownloadFilesQUIC/UploadFilesQUIC need from a
// connection: opening one raw stream on it (xconn.MultiplexedSession), and
// closing it when a worker is done with it. *xconn.QUICSession satisfies
// this directly; proxyRelayConn (see proxyrelay.go) also does, letting
// proxy-mode transfers reuse this exact same client logic while actually
// relaying through the local daemon's persistent connection to the device.
type QUICStreamCloser interface {
	xconn.MultiplexedSession
	Close() error
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
	if err := writeMsg(stream, req); err != nil {
		return nil, err
	}

	var resp fsResponse
	if err := readMsg(stream, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, responseErr(resp)
	}
	return &resp, nil
}

// quicReadWorker owns one QUIC stream for the lifetime of the goroutine
// running it, reusing it across every job pulled from jobs -- opening a
// fresh stream per chunk was measured to badly limit throughput on real
// (non-loopback) links, since the stream never reaches steady-state flow
// control before it's torn down again.
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

	for chunk := range jobs {
		if err := quicReadOneChunk(stream, rootArg, chunk, localPath, localIsDir, sourceRoot, progress); err != nil {
			return err
		}
	}
	return nil
}

func quicReadOneChunk(stream net.Conn, rootArg string, chunk transferChunk, localPath string, localIsDir bool,
	sourceRoot string, progress *transferProgress) error {
	req := fsRequest{Op: fsOpRead, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length}
	if err := writeMsg(stream, req); err != nil {
		return err
	}

	var resp fsResponse
	if err := readMsg(stream, &resp); err != nil {
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
	return copyWithProgress(ow, stream, chunk.Length, progress)
}

// quicWriteWorker is the upload counterpart to quicReadWorker: it owns one
// QUIC stream for the lifetime of the goroutine running it, reusing it
// across every job pulled from jobs.
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

	for chunk := range jobs {
		if err := quicWriteOneChunk(stream, rootArg, chunk, localBase, sourceIsDir, targetIsDirHint,
			progress); err != nil {
			return err
		}
	}
	return nil
}

func quicWriteOneChunk(stream net.Conn, rootArg string, chunk transferChunk, localBase string,
	sourceIsDir, targetIsDirHint bool, progress *transferProgress) error {
	req := fsRequest{
		Op: fsOpWrite, Path: rootArg, RelPath: chunk.RelPath, Offset: chunk.Offset, Length: chunk.Length,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}
	if err := writeMsg(stream, req); err != nil {
		return err
	}

	var ack fsResponse
	if err := readMsg(stream, &ack); err != nil {
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
	if err := copyWithProgress(stream, section, chunk.Length, progress); err != nil {
		return err
	}

	var final fsResponse
	if err := readMsg(stream, &final); err != nil {
		return err
	}
	if !final.OK {
		return responseErr(final)
	}
	return nil
}

// QUICConnector opens one fresh, independent connection on demand.
// DownloadFilesQUIC/UploadFilesQUIC call it once per parallel worker so each
// worker gets its own connection rather than a stream sharing one -- see
// P2PConnector's doc comment for why that matters: streams on one QUIC
// connection share that connection's single congestion-control state.
// *xconn.QUICSession satisfies QUICStreamCloser directly (direct --mode
// quic); proxyRelayConn also does, for proxy-mode transfers relayed
// through the local daemon's persistent connection to the device.
type QUICConnector func() (QUICStreamCloser, error)

// DownloadFilesQUIC downloads remotePath (a file, or if recursive a directory)
// from the device over parallel QUIC streams -- no WAMP call carries any of
// the file's bytes. sess (already connected) serves the one-shot list
// control request; connect opens one independent connection per parallel
// worker. realm is the target device's realm, needed by deskconn-router to
// route each raw stream (see routingFrame). numWorkers <= 0 uses the
// default (parallelStreamWorkers). Thin wrapper around the
// transport-agnostic downloadFiles orchestrator, supplying the QUIC-specific
// list call and per-worker connection.
func DownloadFilesQUIC(sess xconn.MultiplexedSession, connect QUICConnector, realm, remotePath, localPath string,
	recursive bool, numWorkers int) error {
	return downloadFiles(remotePath, localPath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceRoot string, localIsDir bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				workerSess, err := connect()
				if err != nil {
					return err
				}
				defer func() { _ = workerSess.Close() }()
				return quicReadWorker(workerSess, realm, remotePath, jobs, localPath, localIsDir, sourceRoot, progress)
			}
		},
	)
}

// UploadFilesQUIC uploads localPath (a file, or if recursive a directory) to
// the device over parallel QUIC streams -- no WAMP call carries any of the
// file's bytes. sess (already connected) serves the one-shot init control
// request; connect opens one independent connection per parallel worker.
// realm is the target device's realm, needed by deskconn-router to route
// each raw stream (see routingFrame). numWorkers <= 0 uses the default
// (parallelStreamWorkers). Thin wrapper around the transport-agnostic
// uploadFiles orchestrator, supplying the QUIC-specific init call and
// per-worker connection.
func UploadFilesQUIC(sess xconn.MultiplexedSession, connect QUICConnector, realm, localPath, remotePath string,
	recursive bool, numWorkers int) error {
	localBase := filepath.Dir(localPath)
	return uploadFiles(localPath, remotePath, recursive, numWorkers,
		func(req fsRequest) (*fsResponse, error) { return quicRequest(sess, realm, req) },
		func(sourceIsDir, targetIsDirHint bool, progress *transferProgress) func(jobs <-chan transferChunk) error {
			return func(jobs <-chan transferChunk) error {
				workerSess, err := connect()
				if err != nil {
					return err
				}
				defer func() { _ = workerSess.Close() }()
				return quicWriteWorker(workerSess, realm, remotePath, jobs, localBase, sourceIsDir, targetIsDirHint,
					progress)
			}
		},
	)
}
