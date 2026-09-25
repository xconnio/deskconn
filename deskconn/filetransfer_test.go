package deskconn_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/deskconn/deskconn"
)

func TestEffectiveWorkers(t *testing.T) {
	assert.Equal(t, deskconn.ParallelStreamWorkers, deskconn.EffectiveWorkers(0))
	assert.Equal(t, deskconn.ParallelStreamWorkers, deskconn.EffectiveWorkers(-3))
	assert.Equal(t, 1, deskconn.EffectiveWorkers(1))
	assert.Equal(t, 12, deskconn.EffectiveWorkers(12))
	assert.Equal(t, deskconn.MaxStreamWorkers, deskconn.EffectiveWorkers(deskconn.MaxStreamWorkers+100))
}

func TestPlanChunksSplitsLargeFiles(t *testing.T) {
	entries := []common.TransferManifestEntry{
		{RelPath: "dir", IsDir: true},
		{RelPath: "empty.txt", Size: 0},
		{RelPath: "small.txt", Size: 100},
		{RelPath: "exact.txt", Size: deskconn.ParallelChunkSize * 2},
		{RelPath: "big.txt", Size: deskconn.ParallelChunkSize*3 + 17},
	}

	chunks := deskconn.PlanChunks(entries)

	var small, exact, big []deskconn.TransferChunk
	for _, c := range chunks {
		switch c.RelPath {
		case "small.txt":
			small = append(small, c)
		case "exact.txt":
			exact = append(exact, c)
		case "big.txt":
			big = append(big, c)
		case "empty.txt", "dir":
			t.Fatalf("unexpected chunk for %s", c.RelPath)
		}
	}

	require.Len(t, small, 1)
	assert.EqualValues(t, 0, small[0].Offset)
	assert.EqualValues(t, 100, small[0].Length)

	require.Len(t, exact, 2)
	assert.EqualValues(t, deskconn.ParallelChunkSize, exact[0].Length)
	assert.EqualValues(t, deskconn.ParallelChunkSize, exact[1].Length)
	assert.EqualValues(t, deskconn.ParallelChunkSize, exact[1].Offset)

	require.Len(t, big, 4)
	total := int64(0)
	for _, c := range big {
		total += c.Length
	}
	assert.EqualValues(t, deskconn.ParallelChunkSize*3+17, total)
	assert.EqualValues(t, 17, big[3].Length)
}

func TestRunChunkWorkersProcessesEveryChunk(t *testing.T) {
	var mu sync.Mutex
	seen := map[deskconn.TransferChunk]int{}

	chunks := []deskconn.TransferChunk{
		{RelPath: "a", Offset: 0, Length: 10},
		{RelPath: "a", Offset: 10, Length: 10},
		{RelPath: "b", Offset: 0, Length: 5},
	}

	err := deskconn.RunChunkWorkers(chunks, 2, func(jobs <-chan deskconn.TransferChunk) error {
		for c := range jobs {
			mu.Lock()
			seen[c]++
			mu.Unlock()
		}
		return nil
	})
	require.NoError(t, err)

	require.Len(t, seen, 3)
	for c, count := range seen {
		assert.Equal(t, 1, count, "chunk %+v processed %d times", c, count)
	}
}

func TestRunChunkWorkersPropagatesError(t *testing.T) {
	chunks := []deskconn.TransferChunk{
		{RelPath: "a", Offset: 0, Length: 10},
		{RelPath: "b", Offset: 0, Length: 10},
		{RelPath: "c", Offset: 0, Length: 10},
	}

	boom := errors.New("boom")
	err := deskconn.RunChunkWorkers(chunks, 3, func(jobs <-chan deskconn.TransferChunk) error {
		for c := range jobs {
			if c.RelPath == "b" {
				return boom
			}
		}
		return nil
	})
	require.ErrorIs(t, err, boom)
}

func TestRunChunkWorkersEmpty(t *testing.T) {
	called := false
	err := deskconn.RunChunkWorkers(nil, 4, func(jobs <-chan deskconn.TransferChunk) error {
		for range jobs {
			called = true
		}
		return nil
	})
	require.NoError(t, err)
	assert.False(t, called)
}

func TestRunChunkWorkersReusesWorkerAcrossChunks(t *testing.T) {
	var mu sync.Mutex
	setups := 0
	processed := 0

	chunks := make([]deskconn.TransferChunk, 20)
	for i := range chunks {
		chunks[i] = deskconn.TransferChunk{RelPath: "a", Offset: int64(i), Length: 1}
	}

	err := deskconn.RunChunkWorkers(chunks, 2, func(jobs <-chan deskconn.TransferChunk) error {
		mu.Lock()
		setups++
		mu.Unlock()
		for range jobs {
			mu.Lock()
			processed++
			mu.Unlock()
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 20, processed)
	assert.LessOrEqual(t, setups, 2, "each worker should set up once and reuse it across its jobs, not once per chunk")
}

// TestTransferProgressThrottlesPrinting guards against a real throughput
// bug: add() used to print on every call, and for a fine-grained transport
// (16KB WebRTC messages) that's tens of thousands of synchronous stderr
// writes a second on the goroutine reading data off the wire.
func TestTransferProgressThrottlesPrinting(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStderr := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = origStderr
		_ = w.Close()
	}
	defer restore()

	p := deskconn.NewTransferProgress("bench.bin", 1_000_000)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				p.Add(1)
			}
		}()
	}
	wg.Wait()
	p.Finish(nil)

	restore()

	data, readErr := io.ReadAll(r)
	require.NoError(t, readErr)

	count := bytes.Count(data, []byte("\r"))
	assert.Less(t, count, 10, "expected throttled printing, got %d prints for 8000 add() calls", count)
}

// TestTransferProgressFinishReportsActualProgressOnFailure guards against a
// real bug: finish used to unconditionally print 100% even when the
// transfer failed partway through.
func TestTransferProgressFinishReportsActualProgressOnFailure(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	origStderr := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = origStderr
		_ = w.Close()
	}
	defer restore()

	p := deskconn.NewTransferProgress("partial.bin", 1000)
	p.Done.Store(400) // only 400 of 1000 bytes actually made it before the failure
	p.Finish(errors.New("worker failed"))

	restore()

	data, readErr := io.ReadAll(r)
	require.NoError(t, readErr)

	assert.Contains(t, string(data), "400B", "finish() should report the actual bytes completed, not the full total")
	assert.NotContains(t, string(data), "100%", "finish() should not claim 100% when the transfer failed")
}
