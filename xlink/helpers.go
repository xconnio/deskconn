package xlink

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/wampproto-go"
	"github.com/xconnio/xconn-go"
)

// bridgeHandler forwards an invocation into the same-named procedure on
// appSession, chaining progressive results through so streaming procedures
// (e.g. ProcedureFileCat) still work end to end.
func bridgeHandler(appSession *xconn.Session, procedure string) xconn.InvocationHandler {
	return func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		call := appSession.Call(procedure).Args(inv.Args()...).Kwargs(inv.Kwargs())
		if receiveProgress, _ := inv.Details()[wampproto.OptionReceiveProgress].(bool); receiveProgress {
			call = call.ProgressReceiver(func(r *xconn.ProgressResult) {
				_ = inv.SendProgress(r.Args(), r.Kwargs())
			})
		}

		resp := call.Do()
		if resp.Err != nil {
			return xconn.NewInvocationError(common.ErrOperationFailed, resp.Err.Error())
		}
		return xconn.NewInvocationResult(resp.Args()...)
	}
}

// RegisterBridge registers a forwarding handler on session for every
// procedure in deskconn.AppLayerProcedures, relaying each into appSession.
func RegisterBridge(session *xconn.Session, appSession *xconn.Session) error {
	for _, procedure := range AppLayerProcedures {
		resp := session.Register(procedure, bridgeHandler(appSession, procedure)).Invoke(wampproto.InvokeLast).Do()
		if resp.Err != nil {
			return resp.Err
		}
	}
	return nil
}

func EnsureCredentials() (*common.Credentials, error) {
	credFilePath, err := common.CredentialsFilePath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(credFilePath); err != nil {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return nil, fmt.Errorf("failed to create watcher: %w", err)
		}
		defer watcher.Close()

		if err := watcher.Add(filepath.Dir(credFilePath)); err != nil {
			return nil, fmt.Errorf("failed to add watcher: %w", err)
		}

		log.Println("Waiting for credentials file...")

		for event := range watcher.Events {
			if event.Name != credFilePath {
				continue
			}

			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				log.Println("Desktop successfully attached to cloud")
				break
			}
		}
	}

	data, err := os.ReadFile(credFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds common.Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal credentials: %w", err)
	}

	return &creds, nil
}
