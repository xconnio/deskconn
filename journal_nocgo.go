//go:build !cgo

package deskconn

import (
	"errors"
	"time"
)

type journal struct{}

func openJournal() (*journal, error) {
	return nil, errors.New("journal not supported: built without cgo")
}

func (j *journal) close()                          {}
func (j *journal) next() (int, error)              { return 0, nil }
func (j *journal) seekTail() error                 { return nil }
func (j *journal) previousSkip(_ uint64)           {}
func (j *journal) wait(_ time.Duration) bool       { return false }
func (j *journal) addMatch(_ string) error         { return nil }
func (j *journal) seekRealtimeUsec(_ uint64) error { return nil }
func (j *journal) realtimeUsec() uint64            { return 0 }
func (j *journal) fields() map[string]string       { return nil }
