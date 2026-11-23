package terminal

import "context"

// InitialOutputHold is how long a new session's output waits for its first attachment.
const InitialOutputHold = initialOutputHold

// ValidID reports whether id could be a session ID.
func ValidID(id string) bool { return validID(id) }

// Dir is the directory m keeps its sessions' records in.
func (m *Manager) Dir() string { return m.dir }

// SessionIDs lists the sessions with records.
func (m *Manager) SessionIDs() []string {
	ids, _ := recordIDs(m.dir)
	return ids
}

// RecordPath and SocketPath are session id's record and socket.
func (m *Manager) RecordPath(id string) string { return recordPath(m.dir, id) }
func (m *Manager) SocketPath(id string) string { return socketPath(m.dir, id) }

// WorkerPID is the PID session id's record gives for its worker.
func (m *Manager) WorkerPID(id string) (int, error) {
	rec, err := readRecord(m.dir, id)
	return rec.PID, err
}

// AttachWithSecret attaches to session id presenting secret instead of its record's.
func (m *Manager) AttachWithSecret(ctx context.Context, id, secret string) error {
	rec, err := readRecord(m.dir, id)
	if err != nil {
		return err
	}
	rec.Secret = secret
	conn, _, err := m.request(ctx, rec, message{Op: opAttach})
	if conn != nil {
		_ = conn.Close()
	}
	return err
}
