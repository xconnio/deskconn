package common

// agentFwdOp is AgentForwardMsg's discriminator.
type agentFwdOp string

// AgentForwardMsg carries every agent-forward control message. Data itself
// travels as AgentFwdMsgData envelopes tagged via EncodeConnData, not
// through this struct. Op == "" is a ping.
type AgentForwardMsg struct {
	Op     agentFwdOp `json:"op,omitempty"`
	ConnID uint64     `json:"conn_id,omitempty"`
	AuthID string     `json:"auth_id,omitempty"` // start only
	Error  string     `json:"error,omitempty"`   // start ack failure
}
