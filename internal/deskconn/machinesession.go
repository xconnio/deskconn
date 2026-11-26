package deskconn

import (
	"context"

	"github.com/xconnio/deskconn/internal/connectivity"
	"github.com/xconnio/xconn/bindings/go"
)

// MachineSession is a session to call realm's machine on: a connection of the machine's own,
// routed or direct, or, Proxied, the local daemon's session, whose proxy procedures call the
// machine on its persistent connection.
type MachineSession struct {
	*xconn.Session
	Proxied bool
	close   func()
}

// Close ends the session: a routed connection is closed, a direct or the daemon's session left.
func (s *MachineSession) Close() { s.close() }

// ConnectMachineSession connects to realm's machine for mode, as DefaultMode resolved it:
// routed or direct on a connection of its own, otherwise through the local daemon.
func ConnectMachineSession(ctx context.Context, cfgDirectory, realm, mode string) (*MachineSession, error) {
	switch mode {
	case ModeRouted:
		quicSess, err := connectivity.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		return &MachineSession{Session: quicSess.Session, close: func() { _ = quicSess.Connection().Close() }}, nil
	case ModeDirect:
		p2pSess, err := ConnectMachineRealmP2P(ctx, realm, cfgDirectory)
		if err != nil {
			return nil, err
		}
		return &MachineSession{Session: p2pSess, close: func() { _ = p2pSess.Leave() }}, nil
	default:
		localSession, err := connectivity.ConnectDaemon(ctx, cfgDirectory)
		if err != nil {
			return nil, err
		}
		return &MachineSession{Session: localSession, Proxied: true, close: func() { _ = localSession.Leave() }}, nil
	}
}
