package deskconn

import (
	"encoding/json"
	"fmt"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

func aiProxyCall(localSession *xconn.Session, realm, procedure string, payload []byte) ([]byte, error) {
	resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, procedure, payload).Do()
	if resp.Err != nil {
		return nil, resp.Err
	}
	return resp.ArgBytes(0)
}

func parseAISessionListResult(respBytes []byte) ([]common.AISessionSummary, error) {
	var sessions []common.AISessionSummary
	if err := json.Unmarshal(respBytes, &sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return sessions, nil
}

func CallAISessionList(deviceSession *xconn.Session, path string) ([]common.AISessionSummary, error) {
	payload, err := json.Marshal(path)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(deviceSession, common.ProcedureAISessionList, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionListResult(respBytes)
}

func CallAISessionListProxy(localSession *xconn.Session, realm, path string) ([]common.AISessionSummary, error) {
	payload, err := json.Marshal(path)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := aiProxyCall(localSession, realm, common.ProcedureAISessionList, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionListResult(respBytes)
}

func parseAISessionPullResult(respBytes []byte) ([]common.AISessionBundle, error) {
	var bundles []common.AISessionBundle
	if err := json.Unmarshal(respBytes, &bundles); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return bundles, nil
}

func CallAISessionPull(deviceSession *xconn.Session, path, tool, sessionID string) ([]common.AISessionBundle, error) {
	payload, err := json.Marshal(common.AISessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := CallFileOp(deviceSession, common.ProcedureAISessionPull, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionPullResult(respBytes)
}

func CallAISessionPullProxy(localSession *xconn.Session, realm, path, tool,
	sessionID string) ([]common.AISessionBundle, error) {
	payload, err := json.Marshal(common.AISessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	respBytes, err := aiProxyCall(localSession, realm, common.ProcedureAISessionPull, payload)
	if err != nil {
		return nil, err
	}
	return parseAISessionPullResult(respBytes)
}
