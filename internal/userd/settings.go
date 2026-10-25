package userd

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	bolt "go.etcd.io/bbolt"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

const (
	settingsFile = "settings.db"
	// maxSettingSize bounds one setting's value, enough for a frontend's open windows.
	maxSettingSize = 64 << 10
)

// settings are what each user's frontend keeps on the machine (its open windows, dock
// placement...): opaque string values by key, in a bucket per caller authid. The database
// is opened on first use, so a machine whose users never call these never creates it.
type settings struct {
	path string

	mu sync.Mutex
	db *bolt.DB
}

func newSettings(cfgDirectory string) *settings {
	return &settings{path: filepath.Join(cfgDirectory, settingsFile)}
}

func (s *settings) open() (*bolt.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		db, err := openDB(s.path)
		if err != nil {
			return nil, err
		}
		s.db = db
	}
	return s.db, nil
}

func (s *settings) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// values returns all of user's settings, by key.
func (s *settings) values(user string) (map[string]any, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	err = db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(user))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(key, value []byte) error {
			values[string(key)] = string(value)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// put stores user's setting key as value, replacing any it had.
func (s *settings) put(user, key, value string) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(user))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), []byte(value))
	})
}

// settingsCaller is whose settings inv is about, or the error to reply with.
func settingsCaller(inv *xconn.Invocation) (string, *xconn.Result) {
	user := inv.CallerAuthID()
	if user == "" {
		return "", xconn.NewResultError(common.ErrNotAuthorized, "settings are per user: caller has no authid")
	}
	return user, nil
}

// handleList returns all the caller's settings, a dict of key to value.
func (s *settings) handleList(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	user, errResult := settingsCaller(inv)
	if errResult != nil {
		return errResult
	}
	values, err := s.values(user)
	if err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewResult(values)
}

// handleSave stores the caller's setting its first argument names as its second, a string.
func (s *settings) handleSave(_ context.Context, inv *xconn.Invocation) *xconn.Result {
	user, errResult := settingsCaller(inv)
	if errResult != nil {
		return errResult
	}
	key, err := inv.ArgString(0)
	if err != nil || key == "" {
		return xconn.NewResultError(common.ErrInvalidArgument, "want a setting's key and its value")
	}
	value, err := inv.ArgString(1)
	if err != nil {
		return xconn.NewResultError(common.ErrInvalidArgument, "want a setting's key and its value")
	}
	if len(value) > maxSettingSize {
		return xconn.NewResultError(common.ErrInvalidArgument,
			fmt.Sprintf("%s is %d bytes, over %d", key, len(value), maxSettingSize))
	}
	if err := s.put(user, key, value); err != nil {
		return xconn.NewResultError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewResult()
}
