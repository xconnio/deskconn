package userd

import (
	"context"
	"io"
	"net"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// streamHandler binds the feature to the router's procedure authorization.
// Encryption and feature messages run in the stream; routing runs in XConn.
func (d *Deskconn) streamHandler(op common.FSOp, input bool) xconn.InvocationHandler {
	return func(ctx context.Context, inv *xconn.Invocation) *xconn.Result {
		if input {
			stream, err := inv.ArgStream(1)
			if err != nil {
				return xconn.NewResultError(common.ErrInvalidArgument, err.Error())
			}
			connection, ok := stream.(net.Conn)
			if !ok {
				_ = stream.Close()
				return xconn.NewResultError(common.ErrOperationFailed, "stream deadlines unavailable")
			}
			tracked, ok := d.streamConns.add(connection)
			if !ok {
				_ = connection.Close()
				return xconn.NewResultError(common.ErrOperationFailed, "service stopping")
			}
			stop := context.AfterFunc(ctx, func() { _ = tracked.Close() })
			defer stop()
			defer tracked.Close()
			//nolint:contextcheck // RPC cancellation closes the feature connection.
			d.serveStreamOperation(op, &invocationStream{Conn: tracked})
			// Input streams end with the RPC. Keep the invocation alive until
			// the caller has drained the final encrypted reply and sent EOF.
			_, _ = io.Copy(io.Discard, tracked)
			return xconn.NewResult()
		}
		endpoint, worker := net.Pipe()
		tracked, ok := d.streamConns.add(worker)
		if !ok {
			_ = endpoint.Close()
			_ = worker.Close()
			return xconn.NewResultError(common.ErrOperationFailed, "service stopping")
		}
		stop := context.AfterFunc(ctx, func() { _ = tracked.Close() })
		go func() { //nolint:contextcheck // The stream owns this worker after the RPC reply.
			defer stop()
			defer tracked.Close()
			d.serveStreamOperation(op, tracked) //nolint:contextcheck // Stream closure owns the feature lifetime.
		}()
		return xconn.NewResult(nil, xconn.DuplexStream(endpoint))
	}
}

// The invocation owns closure; a one-shot feature must not reset a stream
// while its reply is still travelling to the caller.
type invocationStream struct{ net.Conn }

func (*invocationStream) Close() error { return nil }
