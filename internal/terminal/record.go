package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// record is what a worker writes about its session, for the daemon to find and reach it.
// It's written once the worker is listening and removed as it exits, so a record without a
// listening socket is a worker that died without cleaning up.
type record struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Owner   string    `json:"owner"`
	Command string    `json:"command"`
	Created time.Time `json:"created"`
	PID     int       `json:"pid"` // the worker's
	// Secret authenticates a connection to the worker, as a second check after the
	// terminals directory being private to its user.
	Secret string `json:"secret"`
	// HistoryLimit is the session's share of the history budget (see Manager.Create).
	HistoryLimit int `json:"history_limit,omitempty"`
}

const (
	recordSuffix = ".json"
	socketSuffix = ".sock"
)

// idPattern is a session ID: opaque, and safe to use in a file name.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// newSessionID returns a random session ID.
func newSessionID() (string, error) {
	return randomHex(16)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validID reports whether id could be a session ID, before it's used in a path.
func validID(id string) bool {
	return idPattern.MatchString(id)
}

func recordPath(dir, id string) string { return filepath.Join(dir, id+recordSuffix) }
func socketPath(dir, id string) string { return filepath.Join(dir, id+socketSuffix) }

// ensureDir creates dir, private to this user, and refuses one another user owns or can
// get into: it holds the sockets that control this user's shells.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkDirPrivate(dir)
}

func checkDirPrivate(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if runtime.GOOS == "windows" {
		// Under the user's profile, private to them by its ACL.
		return nil
	}
	if err := checkOwner(info); err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("%s is accessible to other users: %w", dir, err)
		}
	}
	return nil
}

// writeRecord writes rec atomically, readable only by this user.
func writeRecord(dir string, rec record) error {
	data, err := json.Marshal(rec) //nolint:gosec // the secret is for the record, private to its user
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, rec.ID+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), recordPath(dir, rec.ID))
}

func readRecord(dir, id string) (record, error) {
	data, err := os.ReadFile(recordPath(dir, id))
	if err != nil {
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{}, fmt.Errorf("session record %s: %w", id, err)
	}
	if rec.ID != id {
		return record{}, fmt.Errorf("session record %s names another session", id)
	}
	return rec, nil
}

// recordIDs lists the sessions with records in dir.
func recordIDs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), recordSuffix)
		if ok && validID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// removeSession removes a session's record and socket.
func removeSession(dir, id string) {
	_ = os.Remove(recordPath(dir, id))
	_ = os.Remove(socketPath(dir, id))
}
