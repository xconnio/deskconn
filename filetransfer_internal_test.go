package deskconn

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveWorkers(t *testing.T) {
	assert.Equal(t, parallelStreamWorkers, effectiveWorkers(0))
	assert.Equal(t, parallelStreamWorkers, effectiveWorkers(-3))
	assert.Equal(t, 1, effectiveWorkers(1))
	assert.Equal(t, 12, effectiveWorkers(12))
	assert.Equal(t, maxStreamWorkers, effectiveWorkers(maxStreamWorkers+100))
}

func TestBuildManifestSingleFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("hello"), 0644))

	entries, err := buildManifest(filePath, false)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "hello.txt", entries[0].RelPath)
	assert.EqualValues(t, 5, entries[0].Size)
	assert.False(t, entries[0].IsDir)
}

func TestBuildManifestDirWithoutRecursiveFails(t *testing.T) {
	dir := t.TempDir()
	_, err := buildManifest(dir, false)
	require.ErrorContains(t, err, "is a directory")
}

func TestBuildManifestNonExistentPath(t *testing.T) {
	_, err := buildManifest(filepath.Join(t.TempDir(), "nope"), false)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func TestBuildManifestRecursiveDir(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(srcDir, 0755))
	require.NoError(t, os.Mkdir(filepath.Join(srcDir, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested content"), 0644))

	entries, err := buildManifest(srcDir, true)
	require.NoError(t, err)

	byRel := map[string]transferManifestEntry{}
	for _, e := range entries {
		byRel[e.RelPath] = e
	}

	require.Contains(t, byRel, "src")
	assert.True(t, byRel["src"].IsDir)
	require.Contains(t, byRel, "src/sub")
	assert.True(t, byRel["src/sub"].IsDir)
	require.Contains(t, byRel, "src/root.txt")
	assert.EqualValues(t, 4, byRel["src/root.txt"].Size)
	require.Contains(t, byRel, "src/sub/nested.txt")
	assert.EqualValues(t, 14, byRel["src/sub/nested.txt"].Size)
}

func TestPlanChunksSplitsLargeFiles(t *testing.T) {
	entries := []transferManifestEntry{
		{RelPath: "dir", IsDir: true},
		{RelPath: "empty.txt", Size: 0},
		{RelPath: "small.txt", Size: 100},
		{RelPath: "exact.txt", Size: parallelChunkSize * 2},
		{RelPath: "big.txt", Size: parallelChunkSize*3 + 17},
	}

	chunks := planChunks(entries)

	var small, exact, big []transferChunk
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
	assert.EqualValues(t, parallelChunkSize, exact[0].Length)
	assert.EqualValues(t, parallelChunkSize, exact[1].Length)
	assert.EqualValues(t, parallelChunkSize, exact[1].Offset)

	require.Len(t, big, 4)
	total := int64(0)
	for _, c := range big {
		total += c.Length
	}
	assert.EqualValues(t, parallelChunkSize*3+17, total)
	assert.EqualValues(t, 17, big[3].Length)
}

func TestIsRootDir(t *testing.T) {
	dir := t.TempDir()

	existingDir := filepath.Join(dir, "adir")
	require.NoError(t, os.Mkdir(existingDir, 0755))
	existingFile := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(existingFile, []byte("x"), 0644))
	missing := filepath.Join(dir, "missing")

	assert.True(t, isRootDir(existingDir, false, false))
	assert.False(t, isRootDir(existingFile, false, false))
	assert.False(t, isRootDir(missing, false, false))
	assert.True(t, isRootDir(missing, true, false), "sourceIsDir should force directory treatment")
	assert.True(t, isRootDir(missing, false, true), "targetIsDirHint should force directory treatment")
	assert.True(t, isRootDir(existingFile, true, false), "sourceIsDir overrides an existing non-dir target")
}

func TestResolveDestPath(t *testing.T) {
	// rootIsDir: entries nest under root by their full RelPath.
	got := resolveDestPath("/dst", true, "src", "src/sub/file.txt")
	assert.Equal(t, filepath.Clean("/dst/src/sub/file.txt"), got)

	// !rootIsDir: the transfer's top-level entry is renamed to root, and
	// descendants nest under root by the remainder of their RelPath.
	got = resolveDestPath("/dst/renamed", false, "src", "src")
	assert.Equal(t, filepath.Clean("/dst/renamed"), got)

	got = resolveDestPath("/dst/renamed", false, "src", "src/sub/file.txt")
	assert.Equal(t, filepath.Clean("/dst/renamed/sub/file.txt"), got)
}

func TestMaterializeTargetsCreatesDirsAndPreSizedFiles(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	entries := []transferManifestEntry{
		{RelPath: "top", IsDir: true, Mode: 0755},
		{RelPath: "top/sub", IsDir: true, Mode: 0755},
		{RelPath: "top/file.txt", Size: 123, Mode: 0644},
		{RelPath: "top/sub/nested.txt", Size: 7, Mode: 0644},
	}

	require.NoError(t, materializeTargets(entries, dst, true))

	info, err := os.Stat(filepath.Join(dst, "top", "sub"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	fi, err := os.Stat(filepath.Join(dst, "top", "file.txt"))
	require.NoError(t, err)
	assert.EqualValues(t, 123, fi.Size())

	fi, err = os.Stat(filepath.Join(dst, "top", "sub", "nested.txt"))
	require.NoError(t, err)
	assert.EqualValues(t, 7, fi.Size())
}

func TestMaterializeTargetsSingleFileRename(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "renamed.txt")

	entries := []transferManifestEntry{
		{RelPath: "original.txt", Size: 42, Mode: 0644},
	}

	require.NoError(t, materializeTargets(entries, dst, false))

	fi, err := os.Stat(dst)
	require.NoError(t, err)
	assert.EqualValues(t, 42, fi.Size())
}

func TestRunChunkWorkersProcessesEveryChunk(t *testing.T) {
	var mu sync.Mutex
	seen := map[transferChunk]int{}

	chunks := []transferChunk{
		{RelPath: "a", Offset: 0, Length: 10},
		{RelPath: "a", Offset: 10, Length: 10},
		{RelPath: "b", Offset: 0, Length: 5},
	}

	err := runChunkWorkers(chunks, 2, func(jobs <-chan transferChunk) error {
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
	chunks := []transferChunk{
		{RelPath: "a", Offset: 0, Length: 10},
		{RelPath: "b", Offset: 0, Length: 10},
		{RelPath: "c", Offset: 0, Length: 10},
	}

	boom := errors.New("boom")
	err := runChunkWorkers(chunks, 3, func(jobs <-chan transferChunk) error {
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
	err := runChunkWorkers(nil, 4, func(jobs <-chan transferChunk) error {
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

	chunks := make([]transferChunk, 20)
	for i := range chunks {
		chunks[i] = transferChunk{RelPath: "a", Offset: int64(i), Length: 1}
	}

	err := runChunkWorkers(chunks, 2, func(jobs <-chan transferChunk) error {
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

func TestRecvPriorityPrefersBufferedValue(t *testing.T) {
	ch := make(chan int, 1)
	closed := make(chan struct{})
	ch <- 42
	close(closed) // closed is also ready, but the buffered value must win

	v, err := recvPriority(ch, closed, time.Second)
	require.NoError(t, err)
	assert.Equal(t, 42, v)
}

func TestRecvPriorityReturnsErrorOnClose(t *testing.T) {
	ch := make(chan int)
	closed := make(chan struct{})
	close(closed)

	_, err := recvPriority(ch, closed, time.Second)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// TestTransferProgressThrottlesPrinting guards against a real throughput
// bug: printProgress does a synchronous fmt.Fprintf to stderr, and add() is
// called once per network message received/sent -- for a fine-grained
// transport (16KB WebRTC messages) that's tens of thousands of calls a
// second for a large file. Printing on every single one, on the same
// goroutine that's reading data off the wire, throttles real transfer
// throughput down to however fast the terminal can redraw a line whenever
// stderr is at all slow (redirected, tmux, an SSH session).
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

	p := newTransferProgress("bench.bin", 1_000_000)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				p.add(1)
			}
		}()
	}
	wg.Wait()
	p.finish(nil)

	restore()

	data, readErr := io.ReadAll(r)
	require.NoError(t, readErr)

	count := bytes.Count(data, []byte("\r"))
	assert.Less(t, count, 10, "expected throttled printing, got %d prints for 8000 add() calls", count)
}

// TestTransferProgressFinishReportsActualProgressOnFailure guards against a
// real bug: finish used to unconditionally print total/total (100%) even
// when the transfer failed partway through (e.g. one parallel worker's
// connection dropped mid-transfer), which read as success even though the
// destination file was left incomplete.
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

	p := newTransferProgress("partial.bin", 1000)
	p.done.Store(400) // only 400 of 1000 bytes actually made it before the failure
	p.finish(errors.New("worker failed"))

	restore()

	data, readErr := io.ReadAll(r)
	require.NoError(t, readErr)

	assert.Contains(t, string(data), "400B", "finish() should report the actual bytes completed, not the full total")
	assert.NotContains(t, string(data), "100%", "finish() should not claim 100% when the transfer failed")
}
