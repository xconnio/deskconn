//go:build linux

package main

import (
	"context"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-go"
)

func mountFiles(cfgDirectory, target, mountpoint, mode string, readOnly, debug bool) error {
	machine, remotePath := parseMachinePath(target)
	realm, err := machineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	connect := func(ctx context.Context) (*deskconn.MountConnection, error) {
		switch mode {
		case ModeRouted:
			quicSess, err := common.ConnectMachineRealmQUIC(ctx, realm, cfgDirectory)
			if err != nil {
				return nil, err
			}
			return &deskconn.MountConnection{
				Call:  deskconn.DirectMountCaller(quicSess.Session),
				Done:  quicSess.Done(),
				Close: func() { _ = quicSess.Connection().Close() },
			}, nil
		case ModeDirect:
			p2pSess, err := deskconn.ConnectMachineRealmP2P(ctx, realm, cfgDirectory)
			if err != nil {
				return nil, err
			}
			return &deskconn.MountConnection{
				Call:  deskconn.DirectMountCaller(p2pSess),
				Done:  p2pSess.Done(),
				Close: func() { _ = p2pSess.Leave() },
			}, nil
		default:
			localSession, err := xconn.ConnectAnonymous(ctx, common.DaemonURI(cfgDirectory), common.LocalRealm)
			if err != nil {
				return nil, err
			}
			return &deskconn.MountConnection{
				Call:  deskconn.ProxyMountCaller(localSession, realm),
				Done:  localSession.Done(),
				Close: func() { _ = localSession.Leave() },
			}, nil
		}
	}

	call, closeConn, err := deskconn.ReconnectingMountCaller(context.Background(), connect)
	if err != nil {
		return err
	}
	defer closeConn()

	opts := deskconn.MountOptions{ReadOnly: readOnly, Debug: debug}
	return deskconn.Mount(call, remotePath, mountpoint, machine+":"+remotePath, opts)
}
