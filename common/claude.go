package common

import (
	"time"
)

// ClaudeSessionSummary describes one session file found on a machine.
type ClaudeSessionSummary struct {
	Tool      string    `json:"tool"`
	SessionID string    `json:"session_id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int64     `json:"size"`
	// Messages are the session's last chat messages, oldest first.
	Messages []ClaudeMessage `json:"messages,omitempty"`
}

// ClaudeMessage is one chat message of a session: Role is "user" or "assistant".
type ClaudeMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// ClaudeProject is a directory with Claude Code sessions.
type ClaudeProject struct {
	Path      string    `json:"path"`
	Sessions  int       `json:"sessions"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ClaudeSessionListArgs asks for a project's sessions, newest first: Limit of them after
// skipping Offset, or all of them if Limit is 0.
type ClaudeSessionListArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type ClaudeSessionPullArgs struct {
	Path      string `json:"path"`
	Tool      string `json:"tool,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// ClaudeSessionBundle is one tool's bundled session files, as exchanged over the wire.
type ClaudeSessionBundle struct {
	Tool    string `json:"tool"`
	Tarball []byte `json:"tarball"`
}
