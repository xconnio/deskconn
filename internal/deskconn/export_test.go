package deskconn

import "net"

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
