package deskconn

import "fmt"

// defaultCols/defaultRows size the machine's PTY when there's no local terminal to
// take the size from.
const (
	defaultCols = 80
	defaultRows = 24
)

// ExitError is a command run on a machine exiting with a nonzero status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.Code)
}

// ConnectionError is failing to reach a machine, or losing the connection to it, as
// opposed to what ran there failing.
type ConnectionError struct{ Err error }

func (e *ConnectionError) Error() string { return e.Err.Error() }
func (e *ConnectionError) Unwrap() error { return e.Err }
