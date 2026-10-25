package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn-core/bindings/go"
)

// The account service backend: it keeps the accounts and their machines, signs users in with
// a one-time password and tells machines' daemons who may connect.

const (
	procedureAccountLogin       = "io.xconn.deskconn.account.login"
	procedureAccountLoginVerify = "io.xconn.deskconn.account.login.verify"
	procedureAccountGet         = "io.xconn.deskconn.account.get"
	procedurePrincipalDelete    = "io.xconn.deskconn.account.principal.delete"

	procedureDesktopList   = "io.xconn.deskconn.desktop.list"
	procedureDesktopAttach = "io.xconn.deskconn.desktop.attach"
	procedureDesktopDetach = "io.xconn.deskconn.desktop.detach"

	procedureDesktopKeyList      = "io.xconn.deskconn.desktop.access.key.list"
	topicDesktopKeyAddedFormat   = "io.xconn.deskconn.desktop.%s.key.add"
	topicDesktopKeyRemovedFormat = "io.xconn.deskconn.desktop.%s.key.remove"
	topicDesktopDetachFormat     = "io.xconn.deskconn.desktop.%s.detach"
)

func accountServiceCloud() *deskconn.Cloud {
	return &deskconn.Cloud{
		Login:                 accountServiceLogin,
		Name:                  accountServiceName,
		Logout:                accountServiceLogout,
		Machines:              accountServiceMachines,
		Attach:                accountServiceAttach,
		Detach:                accountServiceDetach,
		ProcedureListKeys:     procedureDesktopKeyList,
		TopicKeyAddedFormat:   topicDesktopKeyAddedFormat,
		TopicKeyRemovedFormat: topicDesktopKeyRemovedFormat,
		TopicDetachFormat:     topicDesktopDetachFormat,
	}
}

// connectAccountServiceCRA calls fn on a session signed in to the cloud with username and
// password.
func connectAccountServiceCRA(ctx context.Context, username, password string,
	fn func(session *xconn.Session) error) error {
	quicSess, err := deskconn.ConnectCloudCRA(ctx, username, password)
	if err != nil {
		return err
	}
	common.SafeGo(func() {
		<-quicSess.Done()
		_ = quicSess.Connection().Close()
	})
	defer quicSess.Connection().Close()

	return fn(quicSess.Session)
}

// accountServiceLogin has the account service email username a one-time password, asks for
// it and registers publicKey with it.
func accountServiceLogin(ctx context.Context, username, password, publicKey string) (time.Time, error) {
	var expiresAt time.Time
	err := connectAccountServiceCRA(ctx, username, password, func(session *xconn.Session) error {
		if err := session.Call(procedureAccountLogin).Args(username).Execute().Err; err != nil {
			return err
		}

		otp, err := readOTP()
		if err != nil {
			return err
		}

		callResp := session.Call(procedureAccountLoginVerify).Args(username, otp, publicKey).Execute()
		if callResp.Err != nil {
			return fmt.Errorf("failed to verify otp: %w", callResp.Err)
		}
		principal, err := callResp.ArgDict(0)
		if err != nil {
			return fmt.Errorf("unexpected response from login verify: %w", err)
		}
		expiresAtStr, err := principal.String("expires_at")
		if err != nil {
			return fmt.Errorf("missing expires_at in login verify response: %w", err)
		}
		// One we can't read never expires, as it didn't when the CLI kept it as it came.
		expiresAt, _ = time.Parse(time.RFC3339Nano, expiresAtStr)
		return nil
	})
	return expiresAt, err
}

func accountServiceName(session *xconn.Session) (string, error) {
	callResp := session.Call(procedureAccountGet).Execute()
	if callResp.Err != nil {
		return "", callResp.Err
	}
	account, err := callResp.ArgDict(0)
	if err != nil {
		return "", fmt.Errorf("unexpected response from account get: %w", err)
	}
	name, err := account.String("name")
	if err != nil {
		return "", fmt.Errorf("missing name in account get response: %w", err)
	}
	return name, nil
}

func accountServiceLogout(session *xconn.Session, publicKey string) error {
	return session.Call(procedurePrincipalDelete).Arg(publicKey).Execute().Err
}

func accountServiceMachines(session *xconn.Session) ([]common.Machine, error) {
	callResp := session.Call(procedureDesktopList).Execute()
	if callResp.Err != nil {
		return []common.Machine{}, callResp.Err
	}

	var machines []common.Machine
	jsonData, err := json.Marshal(callResp.Args)
	if err != nil {
		return []common.Machine{}, err
	}
	if err := json.Unmarshal(jsonData, &machines); err != nil {
		return []common.Machine{}, err
	}

	return machines, nil
}

func accountServiceAttach(ctx context.Context, username, password, machineID, publicKey,
	name string) (string, error) {
	var realm string
	err := connectAccountServiceCRA(ctx, username, password, func(session *xconn.Session) error {
		callResp := session.Call(procedureDesktopAttach).Args(machineID, publicKey, name).Execute()
		if callResp.Err != nil {
			return callResp.Err
		}
		respDict, err := callResp.ArgDict(0)
		if err != nil {
			return err
		}
		realm, err = respDict.String("realm")
		return err
	})
	return realm, err
}

func accountServiceDetach(session *xconn.Session, machineID string) error {
	return session.Call(procedureDesktopDetach).Args(machineID).Execute().Err
}
