package deskconn

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xconnio/xconn-go"
)

const (
	ProcedureFileDownload = "io.xconn.deskconn.deskconnd.file.download"

	msgHeader = "H"
	msgData   = "D"

	fileChunkSize = 32 * 1024 // 32 KB
)

func (d *Deskconn) handleFileDownload(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	remotePath, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	recursive, _ := inv.ArgBool(1)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	var resolvedPath string
	if filepath.IsAbs(remotePath) {
		resolvedPath = filepath.Clean(remotePath)
	} else {
		resolvedPath = filepath.Clean(filepath.Join(homeDir, remotePath))
	}

	info, err := os.Lstat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return xconn.NewInvocationError(ErrInvalidArgument,
				fmt.Sprintf("%s: no such file or directory", remotePath))
		}
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	if info.IsDir() && !recursive {
		return xconn.NewInvocationError(ErrInvalidArgument,
			fmt.Sprintf("%s: is a directory, use -r flag to pull directory", remotePath))
	}

	basePath := filepath.Dir(resolvedPath)

	var streamErr error
	if info.IsDir() {
		streamErr = dlStreamDir(inv, resolvedPath, basePath)
	} else {
		streamErr = dlStreamFile(inv, resolvedPath, basePath)
	}
	if streamErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, streamErr.Error())
	}

	return xconn.NewInvocationResult()
}

func dlStreamDir(inv *xconn.Invocation, dirPath, basePath string) error {
	info, err := os.Lstat(dirPath)
	if err != nil {
		return err
	}

	relPath, _ := filepath.Rel(basePath, dirPath)

	if err := inv.SendProgress([]any{msgHeader, map[string]any{
		"name":     info.Name(),
		"rel_path": filepath.ToSlash(relPath),
		"size":     int64(0),
		"mode":     uint32(info.Mode().Perm()),
		"is_dir":   true,
	}}, nil); err != nil {
		return err
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			if err := dlStreamDir(inv, entryPath, basePath); err != nil {
				return err
			}
		} else {
			if err := dlStreamFile(inv, entryPath, basePath); err != nil {
				return err
			}
		}
	}

	return nil
}

func dlStreamFile(inv *xconn.Invocation, filePath, basePath string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", filePath)
	}

	relPath, _ := filepath.Rel(basePath, filePath)

	if err := inv.SendProgress([]any{msgHeader, map[string]any{
		"name":     info.Name(),
		"rel_path": filepath.ToSlash(relPath),
		"size":     info.Size(),
		"mode":     uint32(info.Mode().Perm()),
		"is_dir":   false,
	}}, nil); err != nil {
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := inv.SendProgress([]any{msgData, chunk}, nil); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	return nil
}

func PullFiles(session *xconn.Session, remotePath, localPath string, recursive bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		currentFile     *os.File
		sourceRoot      string
		localIsDir      bool
		transferErr     error
		currentName     string
		currentSize     int64
		currentReceived int64
		currentStart    time.Time
	)

	localInfo, statErr := os.Lstat(localPath)
	localIsDir = statErr == nil && localInfo.IsDir()

	callResp := session.Call(ProcedureFileDownload).
		ProgressReceiver(func(progress *xconn.ProgressResult) {
			if transferErr != nil {
				return
			}

			msgType, err := progress.ArgString(0)
			if err != nil {
				transferErr = fmt.Errorf("invalid message format: %w", err)
				cancel()
				return
			}

			switch msgType {
			case msgHeader:
				// Finalize any open file before starting the next.
				if currentFile != nil {
					_ = currentFile.Close()
					currentFile = nil
					printProgress(currentName, currentSize, currentSize, time.Since(currentStart))
					fmt.Fprintln(os.Stderr)
				}

				headerDict, err := progress.ArgDict(1)
				if err != nil {
					transferErr = fmt.Errorf("invalid file header: %w", err)
					cancel()
					return
				}

				relPath := headerDict.StringOr("rel_path", "")
				name := headerDict.StringOr("name", "")
				size := headerDict.Int64Or("size", 0)
				modeVal := headerDict.UInt64Or("mode", 0)
				isDir := headerDict.BoolOr("is_dir", false)

				if sourceRoot == "" {
					sourceRoot = relPath
				}

				var destPath string
				if localIsDir {
					destPath = filepath.Join(localPath, filepath.FromSlash(relPath))
				} else {
					suffix := strings.TrimPrefix(relPath, sourceRoot)
					destPath = filepath.Clean(localPath + filepath.FromSlash(suffix))
				}

				if isDir {
					perm := os.FileMode(modeVal & 0xFFFFFFFF)
					if perm == 0 {
						perm = 0755
					}
					if err := os.MkdirAll(destPath, perm|0700); err != nil {
						transferErr = err
						cancel()
						return
					}
					fmt.Fprintf(os.Stderr, "%s/\n", name)
				} else {
					if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
						transferErr = err
						cancel()
						return
					}
					perm := os.FileMode(modeVal & 0xFFFFFFFF)
					if perm == 0 {
						perm = 0600
					}
					f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
					if err != nil && os.IsPermission(err) {
						// Existing file may have restrictive permissions (e.g. 0444).
						// handles this by removing the file first, then recreating it.
						if rmErr := os.Remove(destPath); rmErr == nil {
							f, err = os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
						}
					}
					if err != nil {
						transferErr = err
						cancel()
						return
					}
					currentFile = f
					currentName = name
					currentSize = size
					currentReceived = 0
					currentStart = time.Now()
					printProgress(currentName, 0, currentSize, 0)
				}

			case msgData:
				if currentFile == nil {
					transferErr = fmt.Errorf("data received without file header")
					cancel()
					return
				}
				chunk, err := progress.ArgBytes(1)
				if err != nil {
					_ = currentFile.Close()
					currentFile = nil
					transferErr = fmt.Errorf("invalid data chunk: %w", err)
					cancel()
					return
				}
				if _, err := currentFile.Write(chunk); err != nil {
					_ = currentFile.Close()
					currentFile = nil
					transferErr = err
					cancel()
					return
				}
				currentReceived += int64(len(chunk))
				printProgress(currentName, currentReceived, currentSize, time.Since(currentStart))
			}
		}).Args(remotePath, recursive).DoContext(ctx)

	if currentFile != nil {
		_ = currentFile.Close()
		currentFile = nil
		printProgress(currentName, currentSize, currentSize, time.Since(currentStart))
		fmt.Fprintln(os.Stderr)
	}

	if transferErr != nil {
		return transferErr
	}

	if callResp.Err != nil && sourceRoot == "" {
		return callResp.Err
	}

	return nil
}

func printProgress(name string, received, total int64, elapsed time.Duration) {
	const nameWidth = 40
	runes := []rune(name)
	display := name
	if len(runes) > nameWidth {
		display = "..." + string(runes[len(runes)-(nameWidth-3):])
	}

	if total <= 0 {
		fmt.Fprintf(os.Stderr, "\r%-*s   --%%  %s", nameWidth, display, formatBytes(received))
		return
	}

	pct := int64(100) * received / total
	if received >= total {
		pct = 100
	}

	rateStr := ""
	etaStr := "--:--"
	if elapsed.Seconds() >= 0.1 && received > 0 {
		rate := float64(received) / elapsed.Seconds()
		rateStr = formatBytes(int64(rate)) + "/s"
		if received < total {
			sec := int(float64(total-received) / rate)
			etaStr = fmt.Sprintf("%02d:%02d", sec/60, sec%60)
		} else {
			sec := int(elapsed.Seconds())
			etaStr = fmt.Sprintf("%02d:%02d", sec/60, sec%60)
		}
	}

	fmt.Fprintf(os.Stderr, "\r%-*s %3d%% %9s  %-12s  %s",
		nameWidth, display, pct, formatBytes(received), rateStr, etaStr)
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	fb := float64(b)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1fGB", fb/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1fMB", fb/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1fKB", fb/float64(kb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
