package deskconn

import (
	"encoding/json"
	"fmt"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func claudeProxyCall(localSession *xconn.Session, realm, procedure string, payload []byte) ([]byte, error) {
	resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, procedure, payload).Do()
	if resp.Err != nil {
		return nil, resp.Err
	}
	return resp.ArgBytes(0)
}

func parseClaudeSessionListResult(respBytes []byte) ([]common.ClaudeSessionSummary, error) {
	var sessions []common.ClaudeSessionSummary
	if err := json.Unmarshal(respBytes, &sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return sessions, nil
}

func CallClaudeSessionList(machineSession *xconn.Session, path string) ([]common.ClaudeSessionSummary, error) {
	payload, err := json.Marshal(common.ClaudeSessionListArgs{Path: path})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(machineSession, common.ProcedureClaudeSessionList, payload)
	if err != nil {
		return nil, err
	}
	return parseClaudeSessionListResult(respBytes)
}

func CallClaudeSessionListProxy(localSession *xconn.Session, realm,
	path string) ([]common.ClaudeSessionSummary, error) {
	payload, err := json.Marshal(common.ClaudeSessionListArgs{Path: path})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := claudeProxyCall(localSession, realm, common.ProcedureClaudeSessionList, payload)
	if err != nil {
		return nil, err
	}
	return parseClaudeSessionListResult(respBytes)
}

func parseClaudeSessionPullResult(respBytes []byte) ([]common.ClaudeSessionBundle, error) {
	var bundles []common.ClaudeSessionBundle
	if err := json.Unmarshal(respBytes, &bundles); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return bundles, nil
}

func CallClaudeSessionPull(machineSession *xconn.Session, path, tool,
	sessionID string) ([]common.ClaudeSessionBundle, error) {
	payload, err := json.Marshal(common.ClaudeSessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(machineSession, common.ProcedureClaudeSessionPull, payload)
	if err != nil {
		return nil, err
	}
	return parseClaudeSessionPullResult(respBytes)
}

func CallClaudeSessionPullProxy(localSession *xconn.Session, realm, path, tool,
	sessionID string) ([]common.ClaudeSessionBundle, error) {
	payload, err := json.Marshal(common.ClaudeSessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := claudeProxyCall(localSession, realm, common.ProcedureClaudeSessionPull, payload)
	if err != nil {
		return nil, err
	}
	return parseClaudeSessionPullResult(respBytes)
}
