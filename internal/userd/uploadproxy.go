package userd

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn/bindings/go"
)

func ProxyUploadHandler(clients *ClientSessions, directory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, _ := inv.ArgString(0)
		source, _ := inv.ArgString(1)
		destination, _ := inv.ArgString(2)
		workers, err := inv.ArgInt64(3)
		if realm == "" || !filepath.IsAbs(source) || err != nil || workers < 0 || workers > 64 {
			return xconn.NewResultError(common.ErrInvalidArgument, "invalid upload arguments")
		}
		info, err := os.Lstat(source)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		if !info.Mode().IsRegular() {
			return xconn.NewResultError(common.ErrInvalidArgument, "source must be a regular file")
		}
		mode := inv.ArgStringOr(5, "")
		if mode != "" && mode != deskconn.ModeDirect && mode != deskconn.ModeRouted {
			return xconn.NewResultError(common.ErrInvalidArgument, "invalid upload mode")
		}
		session, release, err := uploadSession(ctx, clients, realm, directory, mode)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		defer release()
		var progress io.Writer
		if inv.ArgBoolOr(4, false) {
			progress = uploadProgress{inv}
		}
		if err := deskconn.UploadFilesContext(ctx, session, source, destination, false, int(workers), progress); err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewResult()
	}
}

func uploadSession(ctx context.Context, clients *ClientSessions, realm, directory, mode string) (
	common.StreamSession, func(), error,
) {
	if mode != "" && common.IsStandaloneMachineRealm(realm) {
		if err := common.LoadStandaloneTarget(directory, realm); err != nil {
			return nil, nil, err
		}
	}
	switch mode {
	case deskconn.ModeDirect:
		session, err := deskconn.ConnectMachineRealmP2PSession(ctx, realm, directory)
		if err != nil {
			return nil, nil, err
		}
		return session, func() {
			_ = session.Close()
			_ = session.Connection().Close()
		}, nil
	case deskconn.ModeRouted:
		session, err := connectivity.ConnectMachineRealmQUIC(ctx, realm, directory)
		if err != nil {
			return nil, nil, err
		}
		return session, func() {
			_ = session.Close()
			_ = session.Connection().Close()
		}, nil
	default:
		session, err := clients.EnsureMachineSession(ctx, realm, directory)
		if err != nil {
			return nil, nil, err
		}
		return session, func() {}, nil
	}
}

type uploadProgress struct{ inv *xconn.Invocation }

func (p uploadProgress) Write(data []byte) (int, error) {
	if err := p.inv.SendProgress([]any{string(data)}, nil); err != nil {
		return 0, err
	}
	return len(data), nil
}
