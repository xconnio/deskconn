package deskconnd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// fileChunkSize is the read/write buffer size used by streaming
// progressive-invocation transfers (file cat).
const fileChunkSize = 1024 * 1024 // 1mb

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
		return "", nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	info, err := os.Lstat(resolvedRemotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, xconn.NewInvocationError(common.ErrInvalidArgument,
				fmt.Sprintf("%s: no such file or directory", remotePath))
		}
		return "", nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return resolvedRemotePath, info, nil
}

// serverStreamKeyExchange reads the client's public key from the invocation at clientKeyArgIdx,
// performs an X25519 key exchange, derives both send/receive keys, and sends the server's
// public key back as the first progress message.
func serverStreamKeyExchange(inv *xconn.Invocation, clientKeyArgIdx int) (*encryptionKeys, *xconn.InvocationResult) {
	clientPublicKey, err := inv.ArgBytes(clientKeyArgIdx)
	if err != nil || len(clientPublicKey) != 32 {
		return nil, xconn.NewInvocationError(common.ErrInvalidArgument, "client public key is required")
	}

	serverPublicKey, sendKey, receiveKey, err := ServerKeyExchange(clientPublicKey)
	if err != nil {
		return nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	if err := inv.SendProgress([]any{append([]byte("KEY:"), serverPublicKey...)}, nil); err != nil {
		return nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}

	return &encryptionKeys{sendKey: sendKey, receiveKey: receiveKey}, nil
}
