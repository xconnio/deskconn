package deskconn

import (
	"context"
	"errors"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
)

// DaemonConnectedMachines returns when the local daemon connected to each machine it's
// connected to, by realm.
func DaemonConnectedMachines(ctx context.Context, cfgDirectory string) (map[string]any, error) {
	session, err := connectivity.ConnectDaemon(ctx, cfgDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Leave() }()
	return ConnectedMachines(session)
}

// ConnectedMachines returns, on daemon, the local daemon's session, when it connected to
// each machine it's connected to, by realm.
func ConnectedMachines(daemon *xconn.Session) (map[string]any, error) {
	resp := daemon.Call(common.ProcedureConnectedMachines).Execute()
	if resp.Err != nil {
		return nil, resp.Err
	}
	if len(resp.Args) == 0 {
		return nil, errors.New("expected a map of strings")
	}
	connected, ok := resp.Args[0].(map[string]any)
	if !ok {
		return nil, errors.New("expected a map of strings")
	}
	return connected, nil
}

// MachinePinger returns a func that pings realm's machine once and returns the round trip
// in milliseconds: through the daemon (see ThroughDaemon), otherwise over a connection of its
// own, which the returned close func closes.
func MachinePinger(cfgDirectory, realm string) (func() (int64, error), func(), error) {
	if !ThroughDaemon(cfgDirectory, realm) {
		conn, err := connectivity.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
		if err != nil {
			return nil, nil, err
		}
		return func() (int64, error) {
			start := time.Now()
			if err := conn.Call(common.ProcedurePing).Execute().Err; err != nil {
				return 0, err
			}
			return time.Since(start).Milliseconds(), nil
		}, func() { _ = conn.Connection().Close() }, nil
	}

	localSession, err := connectivity.ConnectDaemon(context.Background(), cfgDirectory)
	if err != nil {
		return nil, nil, err
	}
	return func() (int64, error) {
		callResp := localSession.Call(common.ProcedureProxyPing).Args(realm).Execute()
		if callResp.Err != nil {
			return 0, callResp.Err
		}
		ms, _ := callResp.ArgInt64(0)
		return ms, nil
	}, func() { _ = localSession.Leave() }, nil
}

// FetchMachineInfo returns realm's machine's common.MachineInfo, as JSON, connecting for mode
// as DefaultMode resolved it.
func FetchMachineInfo(cfgDirectory, realm, mode string) ([]byte, error) {
	session, err := ConnectMachineSession(context.Background(), cfgDirectory, realm, mode)
	if err != nil {
		return nil, err
	}
	if session.Proxied {
		callResp := session.Call(common.ProcedureProxyMachineInfo).Args(realm).Execute()
		if callResp.Err != nil {
			return nil, callResp.Err
		}
		return callResp.ArgBytes(0)
	}
	defer session.Close()
	return CallFileOp(session.Session, common.ProcedureMachineInfo, nil)
}

// CallMachine calls procedure with args on machine, routed over QUIC, and returns its
// first result.
func CallMachine(cfgDirectory, machine, procedure string, args ...any) (any, error) {
	realm, err := MachineRealm(machine, cfgDirectory)
	if err != nil {
		return nil, err
	}
	session, err := connectivity.ConnectMachineRealmQUIC(context.Background(), realm, cfgDirectory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	resp := session.Call(procedure).Args(args...).Execute()
	if resp.Err != nil {
		return nil, resp.Err
	}
	if resp.ArgsLen() == 0 {
		return nil, nil
	}
	return resp.Args[0], nil
}
