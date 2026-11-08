package deskconn

import (
	"context"
	"errors"
	"fmt"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

var ErrDaemonUnavailable = errors.New("deskconn-userd unavailable")

// DaemonStreams uses the daemon's cached machine session through ordinary RPCs.
// Each forwarded stream pins its connection until it finishes.
type DaemonStreams struct {
	session *xconn.Session
	realm   string
	direct  bool
}

func DialDaemonStreams(ctx context.Context, realm, directory string) (*DaemonStreams, error) {
	session, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(directory), common.LocalRealm)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	result := session.Call(common.ProcedureProxyStream).Args(realm, "").ExecuteContext(ctx)
	if result.Err != nil {
		_ = session.Close()
		return nil, result.Err
	}
	return &DaemonStreams{session: session, realm: realm, direct: result.ArgBoolOr(0, false)}, nil
}

func (d *DaemonStreams) P2P() bool { return d.direct }

func (d *DaemonStreams) Close() error { return d.session.Close() }

func (d *DaemonStreams) Call(procedure string) *xconn.CallRequest {
	return d.session.Call(common.ProcedureProxyStream).Args(d.realm, procedure)
}
