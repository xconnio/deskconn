package userd

import (
	"context"
	"io"
	"os"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// handleFileCat streams a file's contents. Its argument is the path as an encrypted JSON
// string, and each progress result is one encrypted chunk of the file, both with the
// caller's key-exchange keys.
func (d *Deskconn) handleFileCat(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
	enc, errResult := d.callerKeys(inv)
	if errResult != nil {
		return errResult
	}
	sendKey := enc.sendKey

	var remotePath string
	if errResult = decryptArgs(enc, inv, &remotePath); errResult != nil {
		return errResult
	}

	resolvedPath, info, pathErr := resolveAndStatPath(remotePath)
	if pathErr != nil {
		return pathErr
	}

	if info.IsDir() {
		return xconn.NewResultError(common.ErrInvalidArgument, "path is a directory")
	}

	f, err := os.Open(resolvedPath)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, withoutPath(err).Error())
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			encrypted, err := common.EncryptPayload(chunk, sendKey)
			if err != nil {
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}
			if err := inv.SendProgress([]any{encrypted}, nil); err != nil {
				return xconn.NewResultError(common.ErrOperationFailed, err.Error())
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return xconn.NewResultError(common.ErrOperationFailed, withoutPath(readErr).Error())
		}
	}

	return xconn.NewResult()
}
