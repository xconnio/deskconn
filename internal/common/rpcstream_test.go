package common_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn/bindings/go"
)

func TestRPCStreamInputAndResultEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	router, err := xconn.NewRouter(nil)
	require.NoError(t, err)
	defer router.Close()
	require.NoError(t, router.AddRealm("test", nil))
	callee, err := xconn.ConnectInMemory(router, "test")
	require.NoError(t, err)
	defer callee.Close()
	caller, err := xconn.ConnectInMemory(router, "test")
	require.NoError(t, err)
	defer caller.Close()
	require.NoError(t, callee.Register("input", func(_ context.Context, inv *xconn.Invocation) *xconn.Result {
		stream, openErr := inv.ArgStream(1)
		if openErr != nil {
			return xconn.NewResultError("test.stream_failed", openErr.Error())
		}
		defer stream.Close()
		buffer := make([]byte, 4)
		if _, readErr := io.ReadFull(stream, buffer); readErr != nil {
			return xconn.NewResultError("test.read_failed", readErr.Error())
		}
		if _, writeErr := stream.Write(buffer); writeErr != nil {
			return xconn.NewResultError("test.write_failed", writeErr.Error())
		}
		_, _ = io.Copy(io.Discard, stream)
		return xconn.NewResult()
	}).ExecuteContext(ctx).Err)
	require.NoError(t, callee.Register("output", func(ctx context.Context, _ *xconn.Invocation) *xconn.Result {
		endpoint, worker := net.Pipe()
		stop := context.AfterFunc(ctx, func() { _ = worker.Close() })
		go func() {
			defer stop()
			defer worker.Close()
			buffer := make([]byte, 4)
			if _, readErr := io.ReadFull(worker, buffer); readErr == nil {
				_, _ = worker.Write(buffer)
			}
		}()
		return xconn.NewResult(nil, xconn.DuplexStream(endpoint))
	}).ExecuteContext(ctx).Err)
	for _, procedure := range []string{"input", "output"} {
		stream, openErr := common.OpenRPCStream(ctx, caller, procedure, procedure == "input")
		require.NoError(t, openErr)
		_, writeErr := stream.Write([]byte("ping"))
		require.NoError(t, writeErr)
		buffer := make([]byte, 4)
		_, readErr := io.ReadFull(stream, buffer)
		require.NoError(t, readErr)
		require.Equal(t, "ping", string(buffer))
		require.NoError(t, stream.Close())
	}
	require.NoError(t, callee.Register("denied", func(context.Context, *xconn.Invocation) *xconn.Result {
		return xconn.NewResultError("test.denied")
	}).ExecuteContext(ctx).Err)
	stream, err := common.OpenRPCStream(ctx, caller, "denied", true)
	require.NoError(t, err)
	defer stream.Close()
	_, _ = stream.Write([]byte("x"))
	_, err = stream.Read(make([]byte, 1))
	var application *xconn.Error
	require.ErrorAs(t, err, &application)
	require.Equal(t, "test.denied", application.URI)
}
