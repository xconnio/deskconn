package deskconn

import (
	"io"
	"net"
)

// Aliases exposing unexported transfer internals to the package's external tests.

const (
	MaxStreamWorkers      = maxStreamWorkers
	ParallelStreamWorkers = parallelStreamWorkers
)

var (
	EffectiveWorkers    = effectiveWorkers
	NewTransferProgress = newTransferProgress
	PlanChunks          = planChunks
	RunChunkWorkers     = runChunkWorkers
)

type TransferChunk = transferChunk

func NewShellFrameWriter(stream net.Conn) func([]byte) error {
	conn := &streamClientShellConn{stream: stream}
	return conn.sendEnvelope
}

// RunShellStdin runs the shell client's stdin loop, sending to stream encrypted with key.
func RunShellStdin(stream net.Conn, key []byte, stdin io.Reader, forwardEOF bool) {
	active := &activeShellConn{}
	active.set(&shellHandshakeResult{conn: &streamClientShellConn{stream: stream}, sendKey: key, cleanup: func() {}})
	shellStdinLoop(active, stdin, forwardEOF)
}
