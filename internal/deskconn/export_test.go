package deskconn

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
