package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/deskconn/internal/deskconn"
	"github.com/xconnio/xconn/bindings/go"
)

// The account service backend: it keeps the accounts and their machines, signs users in by
// having them approve the CLI's key in a browser and tells machines' daemons who may connect.

const (
	procedureCLILoginStart   = "com.deskconn.account.login.cli.start"
	procedureCLILoginPoll    = "com.deskconn.account.login.cli.poll"
	procedureAccountGet      = "com.deskconn.account.get"
	procedurePrincipalDelete = "com.deskconn.account.principal.delete"

	procedureDesktopList   = "com.deskconn.desktop.list"
	procedureDesktopAttach = "com.deskconn.desktop.attach"
	procedureDesktopDetach = "com.deskconn.desktop.detach"

	procedureDesktopKeyList      = "com.deskconn.desktop.access.key.list"
	topicDesktopKeyAddedFormat   = "com.deskconn.desktop.%s.key.add"
	topicDesktopKeyRemovedFormat = "com.deskconn.desktop.%s.key.remove"
	topicDesktopDetachFormat     = "com.deskconn.desktop.%s.detach"
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

// deskconn-cli is the identity the CLI joins the cloud with before the user signs in: the
// cloud only lets it start a browser login and poll for its approval. The key ships with
// every build, so it is no secret. Self-hosted builds override it with
// -ldflags "-X main.cliPrivateKey=<hex seed>".
var cliPrivateKey = "b4cbc56d12e574d5f4c0cf653ffb71aa610aa8e4c8bd5923d53aa1e61c458d73" //nolint:gochecknoglobals,gosec

const (
	cliAuthID = "deskconn-cli"
	// loginStatusKey holds a login poll's answer: pending, approved, denied or expired.
	loginStatusKey = "status"
	// maxLoginPollFailures is how many polls in a row may fail before the login gives up.
	maxLoginPollFailures = 3
)

// minLoginPollInterval keeps the CLI from polling nonstop whatever interval the cloud asks for.
var minLoginPollInterval = time.Second //nolint:gochecknoglobals // lowered by tests

// accountServiceLogin has the account service start a browser login for publicKey, hands
// prompt where to approve it, and polls until the user approves or denies it there or it
// expires.
func accountServiceLogin(ctx context.Context, publicKey, hostname string,
	prompt func(url, userCode string)) (string, time.Time, error) {
	session, err := deskconn.ConnectCloudCryptosign(ctx, cliAuthID, cliPrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = session.Leave() }()

	login, err := startCLILogin(ctx, session, publicKey, hostname)
	if err != nil {
		return "", time.Time{}, err
	}
	prompt(login.url, login.userCode)
	return pollCLILogin(ctx, session, login)
}

type cliLogin struct {
	deviceCode, userCode, url string
	interval                  time.Duration
}

func startCLILogin(ctx context.Context, session *xconn.Session, publicKey, hostname string) (cliLogin, error) {
	callResp := session.Call(procedureCLILoginStart).Args(publicKey, hostname).ExecuteContext(ctx)
	if callResp.Err != nil {
		return cliLogin{}, fmt.Errorf("failed to start login: %w", callResp.Err)
	}
	request, err := callResp.ArgDict(0)
	if err != nil {
		return cliLogin{}, fmt.Errorf("unexpected response from login start: %w", err)
	}
	login := cliLogin{interval: max(time.Duration(request.Int64Or("interval", 3))*time.Second, minLoginPollInterval)}
	for key, value := range map[string]*string{
		"device_code": &login.deviceCode, "user_code": &login.userCode, "verification_uri": &login.url,
	} {
		if *value, err = request.String(key); err != nil {
			return cliLogin{}, fmt.Errorf("missing %s in login start response: %w", key, err)
		}
	}
	return login, nil
}

// pollCLILogin waits for the user to answer login, returning the authid and key expiry the
// approval gives.
func pollCLILogin(ctx context.Context, session *xconn.Session, login cliLogin) (string, time.Time, error) {
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return "", time.Time{}, ctx.Err()
		case <-time.After(login.interval):
		}

		callResp := session.Call(procedureCLILoginPoll).Args(login.deviceCode).ExecuteContext(ctx)
		if callResp.Err != nil {
			// The account service restarting: the login stays pending meanwhile. Other errors
			// won't go away by asking again.
			var wampErr *xconn.Error
			retry := errors.As(callResp.Err, &wampErr) && wampErr.URI == common.ErrNoSuchProcedure
			if failures++; retry && failures < maxLoginPollFailures && ctx.Err() == nil {
				continue
			}
			return "", time.Time{}, fmt.Errorf("failed to check login: %w", callResp.Err)
		}
		failures = 0
		result, err := callResp.ArgDict(0)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("unexpected response from login poll: %w", err)
		}
		status, err := result.String(loginStatusKey)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("missing status in login poll response: %w", err)
		}
		switch status {
		case "pending":
		case "denied":
			return "", time.Time{}, errors.New("login was denied in the browser")
		case "expired":
			return "", time.Time{}, errors.New("login expired before it was approved, please try again")
		case "approved":
			authid, err := result.String("authid")
			if err != nil {
				return "", time.Time{}, fmt.Errorf("missing authid in login poll response: %w", err)
			}
			// One we can't read never expires, as it didn't when the CLI kept it as it came.
			expiresAtStr, _ := result.String("expires_at")
			expiresAt, _ := time.Parse(time.RFC3339Nano, expiresAtStr)
			return authid, expiresAt, nil
		default:
			return "", time.Time{}, fmt.Errorf("unexpected login status %q", status)
		}
	}
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

func accountServiceAttach(session *xconn.Session, machineID, publicKey, name string) (string, error) {
	callResp := session.Call(procedureDesktopAttach).Args(machineID, publicKey, name).Execute()
	if callResp.Err != nil {
		return "", callResp.Err
	}
	respDict, err := callResp.ArgDict(0)
	if err != nil {
		return "", err
	}
	return respDict.String("realm")
}

func accountServiceDetach(session *xconn.Session, machineID string) error {
	return session.Call(procedureDesktopDetach).Args(machineID).Execute().Err
}
