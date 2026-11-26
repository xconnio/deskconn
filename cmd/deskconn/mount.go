//go:build linux

package main

import (
	"context"

	"github.com/xconnio/deskconn/internal/deskconn"
)

func mountFiles(cfgDirectory, target, mountpoint, mode string, readOnly, debug bool) error {
	machine, remotePath := deskconn.ParseMachinePath(target)
	realm, err := deskconn.MachineRealm(machine, cfgDirectory)
	if err != nil {
		return err
	}

	mode = deskconn.DefaultMode(cfgDirectory, realm, mode)
	connect := func(ctx context.Context) (*deskconn.MountConnection, error) {
		session, err := deskconn.ConnectMachineSession(ctx, cfgDirectory, realm, mode)
		if err != nil {
			return nil, err
		}
		var call deskconn.MountCaller
		if session.Proxied {
			call = deskconn.ProxyMountCaller(session.Session, realm)
		} else {
			call = deskconn.DirectMountCaller(session.Session)
		}
		return &deskconn.MountConnection{Call: call, Done: session.Done(), Close: session.Close}, nil
	}

	call, closeConn, err := deskconn.ReconnectingMountCaller(context.Background(), connect)
	if err != nil {
		return err
	}
	defer closeConn()

	opts := deskconn.MountOptions{ReadOnly: readOnly, Debug: debug}
	return deskconn.Mount(call, remotePath, mountpoint, machine+":"+remotePath, opts)
}
