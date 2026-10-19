package deskconn

import (
	"encoding/json"
	"fmt"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-go"
)

func agentProxyCall(localSession *xconn.Session, realm, procedure string, payload []byte) ([]byte, error) {
	resp := localSession.Call(common.ProcedureProxyFileOp).Args(realm, procedure, payload).Do()
	if resp.Err != nil {
		return nil, resp.Err
	}
	return resp.ArgBytes(0)
}

func parseAgentSessionListResult(respBytes []byte) ([]common.AgentSessionSummary, error) {
	var sessions []common.AgentSessionSummary
	if err := json.Unmarshal(respBytes, &sessions); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return sessions, nil
}

func CallAgentSessionList(machineSession *xconn.Session, tool, path string) ([]common.AgentSessionSummary, error) {
	payload, err := json.Marshal(common.AgentSessionListArgs{Path: path})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	procedure, _, _ := common.AgentProcedures(tool)
	respBytes, err := CallFileOp(machineSession, procedure, payload)
	if err != nil {
		return nil, err
	}
	return parseAgentSessionListResult(respBytes)
}

func CallAgentSessionListProxy(localSession *xconn.Session, realm, tool,
	path string) ([]common.AgentSessionSummary, error) {
	payload, err := json.Marshal(common.AgentSessionListArgs{Path: path})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	procedure, _, _ := common.AgentProcedures(tool)
	respBytes, err := agentProxyCall(localSession, realm, procedure, payload)
	if err != nil {
		return nil, err
	}
	return parseAgentSessionListResult(respBytes)
}

func parseAgentSessionPullResult(respBytes []byte) ([]common.AgentSessionBundle, error) {
	var bundles []common.AgentSessionBundle
	if err := json.Unmarshal(respBytes, &bundles); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return bundles, nil
}

func CallAgentSessionPull(machineSession *xconn.Session, tool, path,
	sessionID string) ([]common.AgentSessionBundle, error) {
	payload, err := json.Marshal(common.AgentSessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	_, procedure, _ := common.AgentProcedures(tool)
	respBytes, err := CallFileOp(machineSession, procedure, payload)
	if err != nil {
		return nil, err
	}
	return parseAgentSessionPullResult(respBytes)
}

func CallAgentSessionPullProxy(localSession *xconn.Session, realm, tool, path,
	sessionID string) ([]common.AgentSessionBundle, error) {
	payload, err := json.Marshal(common.AgentSessionPullArgs{Path: path, Tool: tool, SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	_, procedure, _ := common.AgentProcedures(tool)
	respBytes, err := agentProxyCall(localSession, realm, procedure, payload)
	if err != nil {
		return nil, err
	}
	return parseAgentSessionPullResult(respBytes)
}
