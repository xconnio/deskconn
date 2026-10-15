package common

import (
	"time"
)

// AgentSessionSummary describes one session file found on a machine.
type AgentSessionSummary struct {
	Tool      string    `json:"tool"`
	SessionID string    `json:"session_id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int64     `json:"size"`
	// Messages are the session's last chat messages, oldest first.
	Messages []AgentMessage `json:"messages,omitempty"`
}

// AgentMessage is one chat message of a session: Role is "user" or "assistant".
type AgentMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// AgentProject is a directory with Claude Code sessions.
type AgentProject struct {
	Path      string    `json:"path"`
	Sessions  int       `json:"sessions"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentSessionListArgs asks for a project's sessions, newest first: Limit of them after
// skipping Offset, or all of them if Limit is 0.
type AgentSessionListArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type AgentSessionPullArgs struct {
	Path      string `json:"path"`
	Tool      string `json:"tool,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// AgentSessionBundle is one tool's bundled session files, as exchanged over the wire.
type AgentSessionBundle struct {
	Tool    string `json:"tool"`
	Tarball []byte `json:"tarball"`
}

// AgentTools are the agents whose sessions deskconn lists, pulls and resumes.
func AgentTools() []string { return []string{ClaudeTool, CodexTool} }

// AgentProcedures returns the procedures serving tool's sessions: each agent is its own app,
// so a machine only has those of the agents installed on it.
func AgentProcedures(tool string) (list, pull, projects string) {
	if tool == CodexTool {
		return ProcedureCodexSessionList, ProcedureCodexSessionPull, ProcedureCodexProjectList
	}
	return ProcedureClaudeSessionList, ProcedureClaudeSessionPull, ProcedureClaudeProjectList
}
