package common

import (
	"time"
)

// AISessionSummary describes one session file found on a device.
type AISessionSummary struct {
	Tool      string    `json:"tool"`
	SessionID string    `json:"session_id"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int64     `json:"size"`
}

type AISessionPullArgs struct {
	Path      string `json:"path"`
	Tool      string `json:"tool,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// AISessionBundle is one tool's bundled session files, as exchanged over the wire.
type AISessionBundle struct {
	Tool    string `json:"tool"`
	Tarball []byte `json:"tarball"`
}
