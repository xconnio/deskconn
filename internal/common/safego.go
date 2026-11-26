package common

import (
	"runtime/debug"

	log "github.com/sirupsen/logrus"
)

// SafeGo runs f in a new goroutine, recovering any panic so a bug in one
// background task (a file transfer, port-forward, shell session, etc.)
// cannot take down the whole process.
func SafeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("recovered panic in goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		f()
	}()
}
