package deskconnd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// fileChunkSize is the read/write buffer size used by streaming
// progressive-invocation transfers (file cat).
const fileChunkSize = 1024 * 1024 // 1mb

// isRootedPath reports whether path should be treated as already rooted (not relative to the
// caller's home directory). filepath.IsAbs alone misses two forms on Windows that a client's
// own path handling can still produce:
//   - a bare drive letter ("C:") - IsAbs requires a trailing separator to recognize the drive's
//     root, but "C:" unambiguously means the same thing (e.g. a file browser listing drives as
//     bare letters and using one directly as the path to browse into)
//   - a POSIX-style rooted path ("/etc/hosts" or "\Users\...") - IsAbs requires a drive letter
//     on Windows and rejects these outright, which a client normalizing paths to forward
//     slashes (or written for the same account's Linux device) can still send
func isRootedPath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if vol := filepath.VolumeName(path); vol != "" && vol == path {
		return true
	}
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`)
}

// cleanRootedPath cleans path, first normalizing a bare Windows drive letter ("C:") to its root
// ("C:\") - filepath.Clean alone leaves "C:" as-is, which os.Open et al. then resolve against
// the process's per-drive working directory instead of the drive's root.
func cleanRootedPath(path string) string {
	if vol := filepath.VolumeName(path); vol != "" && vol == path {
		path += string(filepath.Separator)
	}
	return filepath.Clean(path)
}

// resolvePath resolves a remote path argument (as given by a caller,
// relative to the machine's home directory unless already absolute) to a
// clean absolute path on this machine.
func resolvePath(remotePath string) (string, error) {
	if isRootedPath(remotePath) {
		return cleanRootedPath(remotePath), nil
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
