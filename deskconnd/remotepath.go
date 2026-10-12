package deskconnd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// fileChunkSize is the read/write buffer size used by streaming
// progressive-invocation transfers (file cat).
const fileChunkSize = 1024 * 1024 // 1mb

// resolvePath resolves a remote path argument (as given by a caller,
// relative to the machine's home directory unless already absolute) to a
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
// WAMP-shaped error any invocation handler can return directly. The errors
// leave out the path, since the call's path was sent encrypted.
func resolveAndStatPath(remotePath string) (string, os.FileInfo, *xconn.InvocationResult) {
	resolvedRemotePath, err := resolvePath(remotePath)
	if err != nil {
		return "", nil, xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	info, err := os.Lstat(resolvedRemotePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, xconn.NewInvocationError(common.ErrInvalidArgument, "no such file or directory")
		}
		return "", nil, xconn.NewInvocationError(common.ErrOperationFailed, withoutPath(err).Error())
	}
	return resolvedRemotePath, info, nil
}

// withoutPath strips the path from a file operation's error ("open /a/b: permission
// denied" becomes "permission denied").
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
