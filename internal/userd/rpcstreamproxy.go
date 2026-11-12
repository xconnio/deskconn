package userd

import (
	"context"
	"io"
	"sync"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

// ProxyStreamHandler keeps CLI traffic on the daemon's authenticated machine
// session. It forwards opaque encrypted bytes and pins a routed connection until
// its stream finishes, even if the session upgrades to P2P meanwhile.
func ProxyStreamHandler(clients *ClientSessions, directory string) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		realm, err := inv.ArgString(0)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		procedure, err := inv.ArgString(1)
		if err != nil {
			return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
		}
		input := false
		switch procedure {
		case "":
			_, rtc, connectErr := clients.machineTransports(ctx, realm, directory)
			if connectErr != nil {
				return xconn.NewResultError(common.ErrOperationFailed, connectErr.Error())
			}
			return xconn.NewResult(rtc != nil)
		case common.ProcedureFileInitStream, common.ProcedureFileWriteStream:
			input = true
		case common.ProcedureFileListStream, common.ProcedureFileReadStream, common.ProcedureShellStream,
			common.ProcedurePortForwardStream, common.ProcedurePortReverseStream, common.ProcedureAgentForwardStream,
			common.ProcedureLogsStream:
		default:
			return xconn.NewResultError(common.ErrNotAuthorized, "not a stream endpoint")
		}
		session, err := clients.EnsureMachineSession(ctx, realm, directory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		release := clients.holdSession(realm, session)
		if input {
			defer release()
			stream, openErr := inv.ArgStream(3)
			if openErr != nil {
				return xconn.NewResultError(common.ErrInvalidArgument, openErr.Error())
			}
			defer stream.Close()
			return session.Call(procedure).Arg(nil).Arg(xconn.DuplexStream(stream)).ExecuteContext(ctx)
		}
		result := session.Call(procedure).ExecuteContext(ctx)
		if result.Err != nil {
			release()
			return result
		}
		stream, err := result.ArgStream(1)
		if err != nil {
			release()
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewResult(nil, xconn.DuplexStream(&leasedRPCStream{ReadWriteCloser: stream, release: release}))
	}
}

type leasedRPCStream struct {
	io.ReadWriteCloser
	release func()
	once    sync.Once
}

func (s *leasedRPCStream) CloseWrite() error {
	if half, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return io.ErrClosedPipe
}

func (s *leasedRPCStream) Close() error {
	err := s.ReadWriteCloser.Close()
	s.once.Do(s.release)
	return err
}
