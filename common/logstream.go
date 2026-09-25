package common

// LogControlMsg is the client's one-time request, sent right after key
// exchange. There's no ack: the device just starts streaming (or streams a
// single "error: ...\n" chunk, then closes) immediately.
type LogControlMsg struct {
	Source string `json:"source,omitempty"`
	Follow bool   `json:"follow,omitempty"`
	TailN  int64  `json:"tail_n,omitempty"`
	Since  string `json:"since,omitempty"`
}
