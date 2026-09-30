package deskconn

import "github.com/xconnio/deskconn/common"

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

// ReadAllShellEnvelopes reads channel through the P2P shell connection until it ends.
func ReadAllShellEnvelopes(channel common.MessageChannel) [][]byte {
	closed, _ := common.WebrtcBackpressure(channel)
	conn := newP2PClientShellConn(channel, closed)
	var got [][]byte
	for {
		envelope, err := conn.recvEnvelope()
		if err != nil {
			return got
		}
		got = append(got, envelope)
	}
}
