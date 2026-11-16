package common

import (
	"encoding/json"
)

// shellControlOp is ShellControlMsg's discriminator.
type shellControlOp string

// ShellControlMsg is every control message in the raw-stream shell
// protocol, sent inside the same encrypted envelope (ShellMsgControl kind)
// as everything else on the connection.
type ShellControlMsg struct {
	Op      shellControlOp `json:"op"`
	Cols    uint16         `json:"cols,omitempty"`
	Rows    uint16         `json:"rows,omitempty"`
	AuthID  string         `json:"auth_id,omitempty"`  // size only: self-reported, for agent-forward lookup
	Command string         `json:"command,omitempty"`  // size only: exec's command; empty means an interactive bash shell
	Args    []string       `json:"args,omitempty"`     // size only: exec's arguments to Command
	Dir     string         `json:"dir,omitempty"`      // size only: the directory to start in; empty means home
	OldID   string         `json:"old_id,omitempty"`   // migrate only: shell ID being claimed
	Token   string         `json:"token,omitempty"`    // migrate only: the token issued for OldID
	ShellID string         `json:"shell_id,omitempty"` // server->client ack: the (possibly new) shell ID
	Error   string         `json:"error,omitempty"`    // server->client ack: set instead of ShellID/Token on failure
	// ReportsExit is set in the server->client ack by a machine that sends ShellOpExit
	// once the command exits, so the client can tell a lost connection (no ShellOpExit)
	// from an older machine that never sends one.
	ReportsExit bool `json:"reports_exit,omitempty"`
	ExitCode    *int `json:"exit_code,omitempty"` // exit only: the command's exit status
}

// BuildShellEnvelope encrypts plaintext and prepends kind, ready to hand to
// either transport's raw send.
func BuildShellEnvelope(kind byte, plaintext, key []byte) ([]byte, error) {
	ciphertext, err := EncryptPayload(plaintext, key)
	if err != nil {
		return nil, err
	}
	envelope := make([]byte, 1+len(ciphertext))
	envelope[0] = kind
	copy(envelope[1:], ciphertext)
	return envelope, nil
}

// MustJSON marshals v, panicking on failure -- used only for values (our own
// control-message structs) whose encoding can never actually fail.
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
