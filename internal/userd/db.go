package userd

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// openDB opens the database at path, creating it if needed, waiting at most openTimeout for
// another process to let go of it.
func openDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}
