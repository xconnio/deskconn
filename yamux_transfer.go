package deskconn

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/xconn-go"
)

const (
	yamuxOpUpload   = "upload"
	yamuxOpDownload = "download"

	yamuxFrameHeader = "header"
	yamuxFrameDone   = "done"
)

type streamRequest struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type streamResponse struct {
	OK  bool   `json:"ok"`
	Err string `json:"error,omitempty"`
}

type streamFileFrame struct {
	Type    string `json:"type"`
	Name    string `json:"name,omitempty"`
	RelPath string `json:"rel_path,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	IsDir   bool   `json:"is_dir,omitempty"`
}

func writeMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(b))) // nolint:gosec
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
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// AcceptYamuxStreams runs an accept loop on a YamuxSession, dispatching each server-initiated
// stream (e.g. opened by the cloud router on behalf of a CLI client) to HandleYamuxStream.
func (d *Deskconn) AcceptYamuxStreams(sess *xconn.YamuxSession) {
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go d.HandleYamuxStream(nil, stream)
	}
}

// HandleYamuxStream handles a single raw yamux stream carrying a file transfer request.
func (d *Deskconn) HandleYamuxStream(_ xconn.BaseSession, stream net.Conn) {
	defer stream.Close()

	var req streamRequest
	if err := readMsg(stream, &req); err != nil {
		return
	}

	homeDir, _ := os.UserHomeDir()
	var resolvedPath string
	if filepath.IsAbs(req.Path) {
		resolvedPath = filepath.Clean(req.Path)
	} else {
		resolvedPath = filepath.Clean(filepath.Join(homeDir, req.Path))
	}

	switch req.Op {
	case yamuxOpDownload:
		serveDownload(stream, resolvedPath, req.Recursive)
	case yamuxOpUpload:
		receiveUpload(stream, resolvedPath)
	default:
		_ = writeMsg(stream, streamResponse{OK: false, Err: fmt.Sprintf("unknown op: %s", req.Op)})
	}
}

func serveDownload(stream net.Conn, resolvedPath string, recursive bool) {
	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			_ = writeMsg(stream, streamResponse{OK: false,
				Err: fmt.Sprintf("%s: no such file or directory", resolvedPath)})
		} else {
			_ = writeMsg(stream, streamResponse{OK: false, Err: err.Error()})
		}
		return
	}

	if info.IsDir() && !recursive {
		_ = writeMsg(stream, streamResponse{OK: false,
			Err: fmt.Sprintf("%s: is a directory, use recursive flag", resolvedPath)})
		return
	}

	if err := writeMsg(stream, streamResponse{OK: true}); err != nil {
		return
	}

	basePath := filepath.Dir(resolvedPath)
	if info.IsDir() {
		_ = sendDir(stream, resolvedPath, basePath)
	} else {
		_ = sendFile(stream, resolvedPath, basePath)
	}
	_ = writeMsg(stream, streamFileFrame{Type: yamuxFrameDone})
}

func sendDir(w io.Writer, dirPath, basePath string) error {
	info, err := os.Lstat(dirPath)
	if err != nil {
		return err
	}
	relPath, _ := filepath.Rel(basePath, dirPath)
	if err := writeMsg(w, streamFileFrame{
		Type:    yamuxFrameHeader,
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   true,
	}); err != nil {
		return err
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			if err := sendDir(w, entryPath, basePath); err != nil {
				return err
			}
		} else {
			if err := sendFile(w, entryPath, basePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func sendFile(w io.Writer, filePath, basePath string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	relPath, _ := filepath.Rel(basePath, filePath)
	if err := writeMsg(w, streamFileFrame{
		Type:    yamuxFrameHeader,
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
	}); err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func receiveUpload(stream net.Conn, destRoot string) {
	var currentFile *os.File
	defer func() {
		if currentFile != nil {
			_ = currentFile.Close()
		}
	}()

	destInfo, _ := os.Lstat(destRoot)
	destIsDir := destInfo != nil && destInfo.IsDir()

	for {
		var frame streamFileFrame
		if err := readMsg(stream, &frame); err != nil {
			return
		}
		if frame.Type == yamuxFrameDone {
			return
		}

		if currentFile != nil {
			_ = currentFile.Close()
			currentFile = nil
		}

		destPath := filepath.Join(destRoot, filepath.FromSlash(frame.RelPath))
		if !destIsDir && !frame.IsDir {
			destPath = destRoot
		}

		if frame.IsDir {
			perm := os.FileMode(frame.Mode)
			if perm == 0 {
				perm = 0755
			}
			_ = os.MkdirAll(destPath, perm|0700)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			// drain remaining file bytes so the stream stays in sync
			_, _ = io.CopyN(io.Discard, stream, frame.Size)
			continue
		}

		perm := os.FileMode(frame.Mode)
		if perm == 0 {
			perm = 0600
		}
		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			_, _ = io.CopyN(io.Discard, stream, frame.Size)
			continue
		}
		if _, err = io.CopyN(f, stream, frame.Size); err != nil {
			_ = f.Close()
			return
		}
		_ = f.Close()
	}
}

func PushFilesYamux(sess xconn.MultiplexedSession, localPath, remotePath string, recursive bool) error {
	localInfo, err := os.Lstat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", localPath)
		}
		return err
	}
	if localInfo.IsDir() && !recursive {
		return fmt.Errorf("%s: is a directory, use -r flag", localPath)
	}

	stream, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("failed to open yamux stream: %w", err)
	}
	defer stream.Close()

	if err := writeMsg(stream, streamRequest{Op: yamuxOpUpload, Path: remotePath}); err != nil {
		return fmt.Errorf("failed to send upload request: %w", err)
	}

	basePath := filepath.Dir(localPath)
	if localInfo.IsDir() {
		err = clientSendDir(stream, localPath, basePath)
	} else {
		err = clientSendFile(stream, localPath, basePath)
	}
	if err != nil {
		return err
	}

	return writeMsg(stream, streamFileFrame{Type: yamuxFrameDone})
}

func clientSendDir(w io.Writer, dirPath, basePath string) error {
	info, err := os.Lstat(dirPath)
	if err != nil {
		return err
	}
	relPath, _ := filepath.Rel(basePath, dirPath)
	if err := writeMsg(w, streamFileFrame{
		Type:    yamuxFrameHeader,
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   true,
	}); err != nil {
		return err
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			if err := clientSendDir(w, entryPath, basePath); err != nil {
				return err
			}
		} else {
			if err := clientSendFile(w, entryPath, basePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func clientSendFile(w io.Writer, filePath, basePath string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	relPath, _ := filepath.Rel(basePath, filePath)
	if err := writeMsg(w, streamFileFrame{
		Type:    yamuxFrameHeader,
		Name:    info.Name(),
		RelPath: filepath.ToSlash(relPath),
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
	}); err != nil {
		return err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	start := time.Now()
	written := int64(0)
	buf := make([]byte, fileChunkSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err = w.Write(buf[:n]); err != nil {
				return err
			}
			written += int64(n)
			printProgress(info.Name(), written, info.Size(), time.Since(start))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	printProgress(info.Name(), info.Size(), info.Size(), time.Since(start))
	fmt.Fprintln(os.Stderr)
	return nil
}

func PullFilesYamux(sess xconn.MultiplexedSession, remotePath, localPath string, recursive bool) error {
	stream, err := sess.OpenStream()
	if err != nil {
		return fmt.Errorf("failed to open yamux stream: %w", err)
	}
	defer stream.Close()

	if err := writeMsg(stream, streamRequest{Op: yamuxOpDownload, Path: remotePath, Recursive: recursive}); err != nil {
		return fmt.Errorf("failed to send download request: %w", err)
	}

	var resp streamResponse
	if err := readMsg(stream, &resp); err != nil {
		return fmt.Errorf("failed to read server response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Err)
	}

	localInfo, statErr := os.Lstat(localPath)
	localIsDir := statErr == nil && localInfo.IsDir()

	var (
		sourceRoot      string
		currentFile     *os.File
		currentName     string
		currentSize     int64
		currentReceived int64
		currentStart    time.Time
	)
	defer func() {
		if currentFile != nil {
			_ = currentFile.Close()
		}
	}()

	for {
		var frame streamFileFrame
		if err := readMsg(stream, &frame); err != nil {
			return fmt.Errorf("failed to read file frame: %w", err)
		}
		if frame.Type == yamuxFrameDone {
			if currentFile != nil {
				_ = currentFile.Close()
				currentFile = nil
				printProgress(currentName, currentSize, currentSize, time.Since(currentStart))
				fmt.Fprintln(os.Stderr)
			}
			return nil
		}

		if currentFile != nil {
			_ = currentFile.Close()
			currentFile = nil
			printProgress(currentName, currentSize, currentSize, time.Since(currentStart))
			fmt.Fprintln(os.Stderr)
		}

		if sourceRoot == "" {
			sourceRoot = frame.RelPath
		}

		var destPath string
		if localIsDir {
			destPath = filepath.Join(localPath, filepath.FromSlash(frame.RelPath))
		} else {
			suffix := strings.TrimPrefix(frame.RelPath, sourceRoot)
			destPath = filepath.Clean(localPath + filepath.FromSlash(suffix))
		}

		if frame.IsDir {
			perm := os.FileMode(frame.Mode)
			if perm == 0 {
				perm = 0755
			}
			if err := os.MkdirAll(destPath, perm|0700); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "%s/\n", frame.Name)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		perm := os.FileMode(frame.Mode)
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

		currentFile = f
		currentName = frame.Name
		currentSize = frame.Size
		currentReceived = 0
		currentStart = time.Now()
		printProgress(currentName, 0, currentSize, 0)

		buf := make([]byte, fileChunkSize)
		for currentReceived < frame.Size {
			toRead := int64(len(buf))
			if frame.Size-currentReceived < toRead {
				toRead = frame.Size - currentReceived
			}
			n, readErr := stream.Read(buf[:toRead])
			if n > 0 {
				if _, err := currentFile.Write(buf[:n]); err != nil {
					return err
				}
				currentReceived += int64(n)
				printProgress(currentName, currentReceived, currentSize, time.Since(currentStart))
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return readErr
			}
		}
	}
}
