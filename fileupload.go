package deskconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedureFileUpload = "io.xconn.deskconn.deskconnd.file.upload"
	msgKey              = "K"
	msgInit             = "I"
	msgDone             = "E"
	uploadMaxInFlight   = 8
)

type uploadState struct {
	receiveKey  []byte
	destRoot    string
	destIsFile  bool
	currentFile *os.File
	frames      chan *uploadFrame
	closed      bool
	nextSeq     uint64
	cond        *sync.Cond
	sync.Mutex
}

type uploadFrame struct {
	seq     uint64
	msgType string
	payload []byte
	respCh  chan error
}

type uploadInitMsg struct {
	RemotePath      string `json:"remote_path"`
	SourceIsDir     bool   `json:"source_is_dir"`
	TargetIsDirHint bool   `json:"target_is_dir_hint"`
}

type uploadSessions struct {
	sessions map[uint64]*uploadState
	sync.Mutex
}

type uploadProgressStream struct {
	progressCh chan *xconn.Progress
	creditCh   chan struct{}
	final      *xconn.Progress
}

func newUploadSessions() *uploadSessions {
	return &uploadSessions{sessions: make(map[uint64]*uploadState)}
}

func newUploadProgressStream() *uploadProgressStream {
	creditCh := make(chan struct{}, uploadMaxInFlight)
	for range uploadMaxInFlight {
		creditCh <- struct{}{}
	}

	return &uploadProgressStream{
		progressCh: make(chan *xconn.Progress, uploadMaxInFlight),
		creditCh:   creditCh,
	}
}

func (u *uploadProgressStream) Ack() {
	select {
	case u.creditCh <- struct{}{}:
	default:
	}
}

func sendEncryptedUploadFrame(ctx context.Context, stream *uploadProgressStream, seq *uint64, msgType string,
	payload any, sendKey []byte) error {
	var plaintext []byte
	switch v := payload.(type) {
	case []byte:
		plaintext = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		plaintext = data
	}

	encrypted, err := EncryptPayload(plaintext, sendKey)
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.creditCh:
	}

	err = nil
	select {
	case <-ctx.Done():
		stream.Ack()
		err = ctx.Err()
	case stream.progressCh <- &xconn.Progress{
		Args:    []any{msgType, *seq, encrypted},
		Options: map[string]any{"progress": true},
	}:
	}
	*seq++
	return err
}

func (u *uploadSessions) ensure(callerID uint64, receiveKey []byte) *uploadState {
	u.Lock()
	defer u.Unlock()
	if s, ok := u.sessions[callerID]; ok {
		return s
	}
	s := &uploadState{
		receiveKey: receiveKey,
		frames:     make(chan *uploadFrame, uploadMaxInFlight),
	}
	s.cond = sync.NewCond(&s.Mutex)
	u.sessions[callerID] = s
	go s.processFrames()
	return s
}

func (u *uploadSessions) delete(callerID uint64) {
	u.Lock()
	defer u.Unlock()
	if s, ok := u.sessions[callerID]; ok {
		s.Lock()
		if !s.closed {
			close(s.frames)
			s.closed = true
		}
		s.Unlock()
		delete(u.sessions, callerID)
	}
}

func (s *uploadState) processFrames() {
	defer func() {
		if s.currentFile != nil {
			_ = s.currentFile.Close()
		}
	}()

	pending := make(map[uint64]*uploadFrame)
	nextSeq := uint64(0)

	flush := func() {
		for {
			next, ok := pending[nextSeq]
			if !ok {
				return
			}
			next.respCh <- s.processFrame(next)
			close(next.respCh)
			delete(pending, nextSeq)
			s.Lock()
			s.nextSeq = nextSeq + 1
			s.cond.Broadcast()
			s.Unlock()
			nextSeq++
		}
	}

	for frame := range s.frames {
		pending[frame.seq] = frame
		flush()
	}
	flush()

	// Frames left in pending never reached nextSeq (e.g. the client aborted
	// mid-stream). Unblock their handlers so the invocation goroutines waiting
	// on respCh don't leak.
	for seq, frame := range pending {
		frame.respCh <- fmt.Errorf("upload session closed")
		close(frame.respCh)
		delete(pending, seq)
	}
}

func (s *uploadState) processFrame(frame *uploadFrame) error {
	switch frame.msgType {
	case msgInit:
		pathBytes, err := DecryptPayload(frame.payload, s.receiveKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt remote path: %w", err)
		}

		initMsg := uploadInitMsg{RemotePath: string(pathBytes)}
		if len(pathBytes) > 0 && pathBytes[0] == '{' {
			if err := json.Unmarshal(pathBytes, &initMsg); err != nil {
				return fmt.Errorf("failed to parse upload init: %w", err)
			}
		}

		remotePath := initMsg.RemotePath
		resolvedRemotePath, err := resolvePath(remotePath)
		if err != nil {
			return err
		}

		s.destIsFile = false
		s.destRoot = resolvedRemotePath

		if !initMsg.SourceIsDir {
			if initMsg.TargetIsDirHint {
				return os.MkdirAll(s.destRoot, 0755)
			}
			if info, err := os.Stat(s.destRoot); err == nil && info.IsDir() {
				return fmt.Errorf("target path is an existing directory: %s", s.destRoot)
			} else if err == nil {
				s.destIsFile = true
				return os.MkdirAll(filepath.Dir(s.destRoot), 0755)
			} else if os.IsNotExist(err) {
				s.destIsFile = true
				return os.MkdirAll(filepath.Dir(s.destRoot), 0755)
			} else {
				return err
			}
		}

		return os.MkdirAll(s.destRoot, 0755)

	case msgHeader:
		plaintext, err := DecryptPayload(frame.payload, s.receiveKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt header: %w", err)
		}

		var h fileHeaderMsg
		if err := json.Unmarshal(plaintext, &h); err != nil {
			return fmt.Errorf("failed to parse header: %w", err)
		}

		if s.destRoot == "" {
			return fmt.Errorf("upload not initialized")
		}

		if s.currentFile != nil {
			_ = s.currentFile.Close()
			s.currentFile = nil
		}

		destPath := filepath.Join(s.destRoot, filepath.FromSlash(h.RelPath))
		if s.destIsFile && !h.IsDir {
			destPath = s.destRoot
		}
		if h.IsDir {
			perm := os.FileMode(h.Mode)
			if perm == 0 {
				perm = 0755
			}
			return os.MkdirAll(destPath, perm|0700)
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		perm := os.FileMode(h.Mode)
		if perm == 0 {
			perm = 0600
		}

		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil && os.IsPermission(err) {
			if rmErr := os.Remove(destPath); rmErr == nil {
				f, err = os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			}
		}
		if err != nil {
			return err
		}

		s.currentFile = f
		return nil

	case msgData:
		if s.currentFile == nil {
			return fmt.Errorf("received file data before file header")
		}

		chunk, err := DecryptPayload(frame.payload, s.receiveKey)
		if err != nil {
			return fmt.Errorf("failed to decrypt chunk: %w", err)
		}
		_, err = s.currentFile.Write(chunk)
		return err
	}

	return fmt.Errorf("unknown upload message type: %s", frame.msgType)
}

func (d *Deskconn) handleFileUpload(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	callerID := inv.Caller()
	enc, ok := d.keys.fetch(callerID)

	if inv.Progress() {
		msgType, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, "invalid message type: "+err.Error())
		}

		if !ok {
			if msgType != msgKey {
				return xconn.NewInvocationError(ErrInvalidArgument,
					"first upload progress must perform key exchange")
			}

			var invErr *xconn.InvocationResult
			enc, invErr = serverStreamKeyExchange(inv, 1)
			if invErr != nil {
				return invErr
			}
			d.keys.store(callerID, enc)
			return xconn.NewInvocationError(xconn.ErrNoResult)
		}

		if msgType == msgKey {
			return xconn.NewInvocationError(ErrInvalidArgument, "key exchange already completed")
		}
	} else if !ok {
		return xconn.NewInvocationError(ErrInvalidArgument, "no session keys, call key exchange first")
	}

	if !inv.Progress() {
		msgType, err := inv.ArgString(0)
		if err != nil || msgType != msgDone {
			d.uploads.delete(callerID)
			return xconn.NewInvocationResult()
		}

		finalSeq, err := inv.ArgUInt64(1)
		if err != nil {
			return xconn.NewInvocationError(ErrInvalidArgument, "invalid final upload sequence: "+err.Error())
		}

		state := d.uploads.ensure(callerID, enc.receiveKey)
		state.Lock()
		for state.nextSeq < finalSeq {
			state.cond.Wait()
		}
		state.Unlock()

		d.uploads.delete(callerID)
		return xconn.NewInvocationResult()
	}

	msgType, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, "invalid message type: "+err.Error())
	}

	seq, err := inv.ArgUInt64(1)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, "invalid upload sequence: "+err.Error())
	}

	encrypted, err := inv.ArgBytes(2)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, "missing upload payload: "+err.Error())
	}

	if ctx.Err() != nil {
		return xconn.NewInvocationError(ErrOperationFailed, ctx.Err().Error())
	}

	state := d.uploads.ensure(callerID, enc.receiveKey)
	frame := &uploadFrame{
		seq:     seq,
		msgType: msgType,
		payload: encrypted,
		respCh:  make(chan error, 1),
	}

	state.Lock()
	if state.closed {
		state.Unlock()
		return xconn.NewInvocationError(ErrOperationFailed, "upload session closed")
	}
	state.frames <- frame
	state.Unlock()

	select {
	case err := <-frame.respCh:
		if err != nil {
			return xconn.NewInvocationError(ErrOperationFailed, err.Error())
		}
	case <-ctx.Done():
		return xconn.NewInvocationError(ErrOperationFailed, ctx.Err().Error())
	}
	if err := inv.SendProgress(nil, nil); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationError(xconn.ErrNoResult)
}

func PushFiles(session *xconn.Session, localPath, remotePath string, recursive bool) error {
	return pushFilesInternal(session, "", localPath, remotePath, recursive)
}

func PushFilesViaProxy(session *xconn.Session, realm, localPath, remotePath string, recursive bool) error {
	return pushFilesInternal(session, realm, localPath, remotePath, recursive)
}

func pushFilesInternal(session *xconn.Session, realm, localPath, remotePath string, recursive bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientPublicKey, clientPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	var sendKey []byte
	keyExchangeResult := make(chan error, 1)

	localInfo, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", localPath)
		}
		return err
	}
	if localInfo.IsDir() && !recursive {
		return fmt.Errorf("%s: is a directory, use -r flag to push directory", localPath)
	}

	stream := newUploadProgressStream()
	errCh := make(chan error, 1)

	go func() {
		defer close(stream.progressCh)
		if err := <-keyExchangeResult; err != nil {
			errCh <- err
			return
		}
		var seq uint64

		if err := sendEncryptedUploadFrame(ctx, stream, &seq, msgInit, uploadInitMsg{
			RemotePath:  remotePath,
			SourceIsDir: localInfo.IsDir(),
			TargetIsDirHint: strings.HasSuffix(remotePath, "/") ||
				filepath.Base(remotePath) == "." ||
				filepath.Base(remotePath) == "..",
		}, sendKey); err != nil {
			if !errors.Is(err, context.Canceled) {
				err = fmt.Errorf("failed to send remote path: %w", err)
			}
			errCh <- err
			return
		}

		basePath := filepath.Dir(localPath)
		if localInfo.IsDir() {
			err = uploadStreamDir(ctx, stream, &seq, localPath, basePath, sendKey)
		} else {
			err = uploadStreamFile(ctx, stream, &seq, localPath, basePath, sendKey)
		}
		if err == nil {
			stream.final = xconn.NewFinalProgress(msgDone, seq)
		}
		errCh <- err
	}()

	procedure := ProcedureFileUpload
	if realm != "" {
		procedure = ProcedureProxyFilePush
	}

	firstSent := false
	firstServerMsg := true
	callResp := session.Call(procedure).
		ProgressSender(func(ctx context.Context) *xconn.Progress {
			if !firstSent {
				firstSent = true
				if realm != "" {
					return xconn.NewProgress(realm, msgKey, clientPublicKey)
				}
				return xconn.NewProgress(msgKey, clientPublicKey)
			}
			select {
			case <-ctx.Done():
				return &xconn.Progress{Err: ctx.Err()}
			case p, ok := <-stream.progressCh:
				if !ok {
					if stream.final != nil {
						return stream.final
					}
					return &xconn.Progress{}
				}
				if realm != "" {
					p.Args = append([]any{realm}, p.Args...)
				}
				return p
			}
		}).
		ProgressReceiver(func(result *xconn.ProgressResult) {
			if firstServerMsg {
				firstServerMsg = false
				if len(result.Args()) == 0 {
					keyExchangeResult <- fmt.Errorf("missing key exchange response")
					cancel()
					return
				}

				data, ok := result.Args()[0].([]byte)
				if !ok {
					keyExchangeResult <- fmt.Errorf("invalid key exchange response")
					cancel()
					return
				}
				if len(data) < 4 || string(data[:4]) != "KEY:" {
					keyExchangeResult <- fmt.Errorf("unexpected key exchange response")
					cancel()
					return
				}

				var err error
				sendKey, _, err = ClientKeyExchangeKeys(clientPrivateKey, data[4:])
				if err != nil {
					keyExchangeResult <- fmt.Errorf("key exchange failed: %w", err)
					cancel()
					return
				}
				keyExchangeResult <- nil
				return
			}
			stream.Ack()
		}).
		DoContext(ctx)

	cancel()

	if firstSent && firstServerMsg {
		keyExchangeResult <- callResp.Err
	}

	uploadErr := <-errCh

	if callResp.Err != nil && errors.Is(uploadErr, context.Canceled) {
		uploadErr = nil
	}
	if uploadErr != nil {
		return uploadErr
	}
	if callResp.Err != nil {
		return callResp.Err
	}
	return nil
}

func uploadStreamDir(ctx context.Context, stream *uploadProgressStream, seq *uint64, dirPath, basePath string,
	sendKey []byte) error {
	info, err := os.Lstat(dirPath)
	if err != nil {
		return err
	}

	relPath, _ := filepath.Rel(basePath, dirPath)

	if err := sendEncryptedUploadFrame(ctx, stream, seq, msgHeader, fileHeaderMsg{
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Size:    0,
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   true,
	}, sendKey); err != nil {
		return err
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			if err := uploadStreamDir(ctx, stream, seq, entryPath, basePath, sendKey); err != nil {
				return err
			}
		} else {
			if err := uploadStreamFile(ctx, stream, seq, entryPath, basePath, sendKey); err != nil {
				return err
			}
		}
	}
	return nil
}

func uploadStreamFile(ctx context.Context, stream *uploadProgressStream, seq *uint64, filePath, basePath string,
	sendKey []byte) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", filePath)
	}

	relPath, _ := filepath.Rel(basePath, filePath)

	if err := sendEncryptedUploadFrame(ctx, stream, seq, msgHeader, fileHeaderMsg{
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   false,
	}, sendKey); err != nil {
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	currentReceived := int64(0)
	currentStart := time.Now()

	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := sendEncryptedUploadFrame(ctx, stream, seq, msgData, buf[:n], sendKey); err != nil {
				return err
			}
			currentReceived += int64(n)
			printProgress(info.Name(), currentReceived, info.Size(), time.Since(currentStart))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	printProgress(info.Name(), info.Size(), info.Size(), time.Since(currentStart))
	fmt.Fprintln(os.Stderr)
	return nil
}
