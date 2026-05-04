package deskconn

import (
	"bytes"
	"context"
	"encoding/json"
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

	fileChunkSize = 1024 * 1024 // 1mb

	name = "name"
)

type fileHeaderMsg struct {
	Name    string `json:"name"`
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	IsDir   bool   `json:"is_dir"`
}

func (d *Deskconn) handleFileDownload(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	remotePath, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(ErrInvalidArgument, err.Error())
	}

	recursive, _ := inv.ArgBool(1)

	clientPublicKey, err := inv.ArgBytes(2)
	if err != nil || len(clientPublicKey) != 32 {
		return xconn.NewInvocationError(ErrInvalidArgument, "client public key is required")
	}
	serverPublicKey, serverPrivateKey, err := CreateX25519KeyPair()
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	sharedSecret, err := PerformKeyExchange(serverPrivateKey, clientPublicKey)
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	sendKey, err := DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
	if err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	if err := inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil); err != nil {
		return xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

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
		streamErr = dlStreamDir(inv, resolvedPath, basePath, sendKey)
	} else {
		streamErr = dlStreamFile(inv, resolvedPath, basePath, sendKey)
	}
	if streamErr != nil {
		return xconn.NewInvocationError(ErrOperationFailed, streamErr.Error())
	}

	return xconn.NewInvocationResult()
}

func dlSendProgress(inv *xconn.Invocation, sendKey []byte, msgType string, payload any) error {
	var plaintext []byte
	switch v := payload.(type) {
	case []byte:
		plaintext = v
	default:
		var err error
		plaintext, err = json.Marshal(v)
		if err != nil {
			return err
		}
	}
	encrypted, err := EncryptPayload(plaintext, sendKey)
	if err != nil {
		return err
	}
	return inv.SendProgress([]any{msgType, encrypted}, nil)
}

func dlStreamDir(inv *xconn.Invocation, dirPath, basePath string, sendKey []byte) error {
	info, err := os.Lstat(dirPath)
	if err != nil {
		return err
	}

	relPath, _ := filepath.Rel(basePath, dirPath)

	if err := dlSendProgress(inv, sendKey, msgHeader, map[string]any{
		name:       info.Name(),
		"rel_path": filepath.ToSlash(relPath),
		"size":     int64(0),
		"mode":     uint32(info.Mode().Perm()),
		"is_dir":   true,
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
			if err := dlStreamDir(inv, entryPath, basePath, sendKey); err != nil {
				return err
			}
		} else {
			if err := dlStreamFile(inv, entryPath, basePath, sendKey); err != nil {
				return err
			}
		}
	}

	return nil
}

func dlStreamFile(inv *xconn.Invocation, filePath, basePath string, sendKey []byte) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", filePath)
	}

	relPath, _ := filepath.Rel(basePath, filePath)

	if err := dlSendProgress(inv, sendKey, msgHeader, map[string]any{
		name:       info.Name(),
		"rel_path": filepath.ToSlash(relPath),
		"size":     info.Size(),
		"mode":     uint32(info.Mode().Perm()),
		"is_dir":   false,
	}); err != nil {
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
			if err := dlSendProgress(inv, sendKey, msgData, chunk); err != nil {
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

	publicKey, privateKey, err := CreateX25519KeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate keypair: %w", err)
	}

	var (
		receiveKey      []byte
		firstServerMsg  = true
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

			if firstServerMsg {
				firstServerMsg = false
				data, argErr := progress.ArgBytes(0)
				if argErr != nil || !bytes.HasPrefix(data, []byte("KEY:")) {
					transferErr = fmt.Errorf("expected key exchange message from server")
					cancel()
					return
				}
				sharedSecret, err := PerformKeyExchange(privateKey, data[4:])
				if err != nil {
					transferErr = fmt.Errorf("key exchange failed: %w", err)
					cancel()
					return
				}
				receiveKey, err = DeriveKeyHKDF(sharedSecret, []byte("backendToFrontend"))
				if err != nil {
					transferErr = fmt.Errorf("key derivation failed: %w", err)
					cancel()
					return
				}
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
				if currentFile != nil {
					_ = currentFile.Close()
					currentFile = nil
					printProgress(currentName, currentSize, currentSize, time.Since(currentStart))
					fmt.Fprintln(os.Stderr)
				}

				encrypted, err := progress.ArgBytes(1)
				if err != nil {
					transferErr = fmt.Errorf("invalid encrypted header: %w", err)
					cancel()
					return
				}
				plaintext, err := DecryptPayload(encrypted, receiveKey)
				if err != nil {
					transferErr = fmt.Errorf("failed to decrypt header: %w", err)
					cancel()
					return
				}
				var h fileHeaderMsg
				if err := json.Unmarshal(plaintext, &h); err != nil {
					transferErr = fmt.Errorf("failed to parse header: %w", err)
					cancel()
					return
				}
				name, relPath, size, modeVal, isDir := h.Name, h.RelPath, h.Size, uint64(h.Mode), h.IsDir

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

				encrypted, err := progress.ArgBytes(1)
				if err != nil {
					_ = currentFile.Close()
					currentFile = nil
					transferErr = fmt.Errorf("invalid encrypted chunk: %w", err)
					cancel()
					return
				}
				chunk, err := DecryptPayload(encrypted, receiveKey)
				if err != nil {
					_ = currentFile.Close()
					currentFile = nil
					transferErr = fmt.Errorf("failed to decrypt chunk: %w", err)
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
		}).Args(remotePath, recursive, publicKey).DoContext(ctx)

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
