package userd

import (
	"context"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

// ProxyStreamHandler keeps CLI traffic on the daemon's authenticated machine
// session. It forwards opaque encrypted bytes; each stream stays on the path XConn chose
// for it when it started. Without a procedure, it reports whether new calls go direct.
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
			direct, connectErr := clients.directPath(ctx, realm, directory)
			if connectErr != nil {
				return xconn.NewResultError(common.ErrOperationFailed, connectErr.Error())
			}
			return xconn.NewResult(direct)
		case common.ProcedureFileInitStream, common.ProcedureFileWriteStream:
			input = true
		case common.ProcedureFileListStream, common.ProcedureFileReadStream, common.ProcedureShellStream,
			common.ProcedurePortForwardStream, common.ProcedurePortReverseStream, common.ProcedureAgentForwardStream,
			common.ProcedureLogsStream, common.ProcedureTerminalStream:
		default:
			return xconn.NewResultError(common.ErrNotAuthorized, "not a stream endpoint")
		}
		session, err := clients.EnsureMachineSession(ctx, realm, directory)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		if input {
			stream, openErr := inv.ArgStream(3)
			if openErr != nil {
				return xconn.NewResultError(common.ErrInvalidArgument, openErr.Error())
			}
			defer stream.Close()
			return session.Call(procedure).Arg(nil).Arg(xconn.DuplexStream(stream)).ExecuteContext(ctx)
		}
		result := session.Call(procedure).ExecuteContext(ctx)
		if result.Err != nil {
			return result
		}
		stream, err := result.ArgStream(1)
		if err != nil {
			return xconn.NewResultError(common.ErrOperationFailed, err.Error())
		}
		return xconn.NewResult(nil, xconn.DuplexStream(stream))
	}
}
