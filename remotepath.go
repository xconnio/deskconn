package deskconn

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xconnio/xconn-go"
)

// msgHeader/msgData are the message-type discriminators used by several
// unrelated progressive-invocation protocols in this package (file cat,
// agent forward, port forward/reverse) to distinguish a header/control
// frame from a data frame.
const (
	msgHeader = "H"
	msgData   = "D"

	// fileChunkSize is the read/write buffer size used by streaming
	// progressive-invocation transfers (file cat, and copyWithProgress in
	// quictransfer.go).
	fileChunkSize = 1024 * 1024 // 1mb

	// name is the "name" JSON map key shared by a couple of handlers
	// (printer list) that build ad hoc map[string]any results.
	name = "name"
)

// resolvePath resolves a remote path argument (as given by a caller,
// relative to the device's home directory unless already absolute) to a
// clean absolute path on this machine.
func resolvePath(remotePath string) (string, error) {
	if filepath.IsAbs(remotePath) {
		return filepath.Clean(remotePath), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(homeDir, remotePath)), nil
}

// resolveAndStatPath resolves remotePath and stats it, returning a
// WAMP-shaped error any invocation handler can return directly.
func resolveAndStatPath(remotePath string) (string, os.FileInfo, *xconn.InvocationResult) {
	resolvedRemotePath, err := resolvePath(remotePath)
	if err != nil {
		return "", nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	info, err := os.Lstat(resolvedRemotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, xconn.NewInvocationError(ErrInvalidArgument,
				fmt.Sprintf("%s: no such file or directory", remotePath))
		}
		return "", nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}
	return resolvedRemotePath, info, nil
}

// serverStreamKeyExchange reads the client's public key from the invocation at clientKeyArgIdx,
// performs an X25519 key exchange, derives both send/receive keys, and sends the server's
// public key back as the first progress message.
func serverStreamKeyExchange(inv *xconn.Invocation, clientKeyArgIdx int) (*encryptionKeys, *xconn.InvocationResult) {
	clientPublicKey, err := inv.ArgBytes(clientKeyArgIdx)
	if err != nil || len(clientPublicKey) != 32 {
		return nil, xconn.NewInvocationError(ErrInvalidArgument, "client public key is required")
	}

	serverPublicKey, sendKey, receiveKey, err := ServerKeyExchange(clientPublicKey)
	if err != nil {
		return nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	if err := inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil); err != nil {
		return nil, xconn.NewInvocationError(ErrOperationFailed, err.Error())
	}

	return &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey}, nil
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
