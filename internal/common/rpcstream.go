package common

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xconnio/xconn-core/bindings/go"
)

// StreamSession is the ordinary fluent RPC API shared by native sessions and
// the local daemon's forwarding session. Features do not select transports.
type StreamSession interface {
	Call(string) *xconn.CallRequest
}

// OpenRPCStream opens an endpoint's positional byte stream. Upload endpoints
// accept a duplex source at argument 1; other endpoints return it at result 1.
// Feature protocols, including end-to-end key exchange, run inside that stream.
func OpenRPCStream(ctx context.Context, session StreamSession, procedure string, input bool) (net.Conn, error) {
	if input {
		lifetime, cancel := context.WithCancel(ctx)
		endpoint, local := net.Pipe()
		stream := &inputRPCStream{Conn: local, cancel: cancel, ctx: lifetime, done: make(chan struct{})}
		go func() {
			defer close(stream.done)
			defer endpoint.Close()
			stream.result = session.Call(procedure).Arg(nil).Arg(xconn.DuplexStream(endpoint)).ExecuteContext(lifetime)
		}()
		return stream, nil
	}
	result := session.Call(procedure).ExecuteContext(ctx)
	if result.Err != nil {
		return nil, result.Err
	}
	stream, err := result.ArgStream(1)
	if err != nil {
		return nil, err
	}
	connection, ok := stream.(net.Conn)
	if !ok {
		_ = stream.Close()
		return nil, fmt.Errorf("RPC stream does not provide connection deadlines")
	}
	return connection, nil
}

type inputRPCStream struct {
	net.Conn
	cancel   context.CancelFunc
	ctx      context.Context
	done     chan struct{}
	result   *xconn.Result
	once     sync.Once
	closeErr error
}

func (s *inputRPCStream) operationError(err error) error {
	if err == nil {
		return nil
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return err
	}
	select {
	case <-s.done:
		if s.result.Err != nil {
			return s.result.Err
		}
		return err
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *inputRPCStream) Read(buffer []byte) (int, error) {
	n, err := s.Conn.Read(buffer)
	return n, s.operationError(err)
}

func (s *inputRPCStream) Write(buffer []byte) (int, error) {
	n, err := s.Conn.Write(buffer)
	return n, s.operationError(err)
}

func (s *inputRPCStream) Close() error {
	s.once.Do(func() {
		s.closeErr = s.Conn.Close()
		defer s.cancel()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
			s.closeErr = errors.Join(s.closeErr, s.result.Err)
		case <-s.ctx.Done():
			s.closeErr = errors.Join(s.closeErr, s.ctx.Err())
		case <-timer.C:
			s.closeErr = errors.Join(s.closeErr, context.DeadlineExceeded)
		}
	})
	return s.closeErr
}
