package deskconnd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func (d *Deskconn) handleFileCat(ctx context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
	remotePath, err := inv.ArgString(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	enc, invErr := serverStreamKeyExchange(inv, 1)
	if invErr != nil {
		return invErr
	}
	sendKey := enc.sendKey

	resolvedPath, info, pathErr := resolveAndStatPath(remotePath)
	if pathErr != nil {
		return pathErr
	}

	if info.IsDir() {
		return xconn.NewInvocationError(common.ErrInvalidArgument,
			fmt.Sprintf("%s: is a directory", remotePath))
	}

	f, err := os.Open(resolvedPath)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	defer f.Close()

	buf := make([]byte, fileChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			encrypted, err := common.EncryptPayload(chunk, sendKey)
			if err != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
			}
			if err := inv.SendProgress([]any{encrypted}, nil); err != nil {
				return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, readErr.Error())
		}
	}

	return xconn.NewInvocationResult()
}
