package deskconn

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xconnio/deskconn/common"
)

const (
	// ParallelChunkSize is the byte range each parallel worker requests (or
	// pushes) from a single file before moving on to the next chunk in the
	// queue -- the unit of work for the worker pool, not the wire message size.
	ParallelChunkSize = 4 * 1024 * 1024 // 4MB

	// parallelStreamWorkers is the default number of chunk requests that run
	// concurrently over separate raw streams/channels, mirroring the
	// parallel range fetches a video player makes against a single source.
	// Callers can override it per transfer -- see effectiveWorkers.
	parallelStreamWorkers = 4

	// maxStreamWorkers caps how many concurrent streams/channels a transfer
	// may open, however it was requested to. Chosen as a sane ceiling
	// against fat-fingered input or a hostile caller, not a value anyone
	// should expect to be efficient -- most links saturate well below it.
	maxStreamWorkers = 64
)

// effectiveWorkers clamps a caller-requested worker count to a sane range:
// n <= 0 falls back to the default (parallelStreamWorkers), and anything
// above maxStreamWorkers is capped there.
func effectiveWorkers(n int) int {
	if n <= 0 {
		return parallelStreamWorkers
	}
	if n > maxStreamWorkers {
		return maxStreamWorkers
	}
	return n
}

// transferChunk is one byte-range job in the flattened work queue the
// parallel workers draw from.
type transferChunk struct {
	RelPath string
	Offset  int64
	Length  int64
}

// planChunks flattens manifest file entries into byte-range jobs of at most
// ParallelChunkSize each. Directories and zero-length files need no data
// transfer -- MaterializeTargets alone accounts for them.
func planChunks(entries []common.TransferManifestEntry) []transferChunk {
	var chunks []transferChunk
	for _, e := range entries {
		if e.IsDir || e.Size == 0 {
			continue
		}
		for off := int64(0); off < e.Size; off += ParallelChunkSize {
			length := int64(ParallelChunkSize)
			if off+length > e.Size {
				length = e.Size - off
			}
			chunks = append(chunks, transferChunk{RelPath: e.RelPath, Offset: off, Length: length})
		}
	}
	return chunks
}

// totalSize sums the size of every non-directory entry.
func totalSize(entries []common.TransferManifestEntry) int64 {
	var total int64
	for _, e := range entries {
		if !e.IsDir {
			total += e.Size
		}
	}
	return total
}

// runChunkWorkers starts up to workers goroutines, each running workerFn
// once against a shared jobs channel. workerFn is expected to open one
// connection up front and reuse it across every job it pulls off jobs,
// rather than opening a fresh one per chunk -- reopening per chunk was
// measured to badly limit throughput on real (non-loopback) links. Stops
// feeding new jobs and returns the first error encountered; workers already
// mid-job are allowed to finish it before exiting.
func runChunkWorkers(chunks []transferChunk, workers int, workerFn func(jobs <-chan transferChunk) error) error {
	if len(chunks) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(chunks) {
		workers = len(chunks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobCh := make(chan transferChunk)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		common.SafeGo(func() {
			defer wg.Done()
			if err := workerFn(jobCh); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
			}
		})
	}

feed:
	for _, c := range chunks {
		select {
		case jobCh <- c:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobCh)
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func newTransferProgress(name string, total int64) *common.TransferProgress {
	p := &common.TransferProgress{Name: name, Total: total, Start: time.Now()}
	p.LastPrint.Store(p.Start.UnixNano())
	common.PrintProgress(p.Name, 0, p.Total, 0)
	return p
}

// downloadFiles is the single reusable download orchestrator every raw
// transport's DownloadFilesXXX wraps (filetransferp2p.go's
// DownloadFilesP2P, quictransfer.go's DownloadFilesQUIC): list the remote
// source via listFn, pre-create local targets, plan chunks, and run
// numWorkers parallel workers built by newWorker. numWorkers <= 0 uses the
// default (parallelStreamWorkers).
//
// listFn performs the one-shot manifest request. newWorker is called once,
// with the manifest's source root and whether localPath should be treated
// as a directory, and must return the per-worker job-queue handler to pass
// to runChunkWorkers.
func downloadFiles(remotePath, localPath string, recursive bool, numWorkers int,
	listFn func(common.FSRequest) (*common.FSResponse, error),
	newWorker func(sourceRoot string, localIsDir bool,
		progress *common.TransferProgress) func(jobs <-chan transferChunk) error,
) error {
	resp, err := listFn(common.FSRequest{Op: common.FSOpList, Path: remotePath, Recursive: recursive})
	if err != nil {
		return err
	}
	entries := resp.Entries
	if len(entries) == 0 {
		return fmt.Errorf("%s: no such file or directory", remotePath)
	}

	localIsDir := common.IsRootDir(localPath, entries[0].IsDir, false)
	if err := common.MaterializeTargets(entries, localPath, localIsDir); err != nil {
		return err
	}

	chunks := planChunks(entries)
	total := totalSize(entries)
	if total == 0 {
		return nil
	}

	progress := newTransferProgress(filepath.Base(remotePath), total)
	err = runChunkWorkers(chunks, effectiveWorkers(numWorkers), newWorker(entries[0].RelPath, localIsDir, progress))
	progress.Finish(err)
	return err
}

// uploadFiles is the upload counterpart to downloadFiles: the single
// reusable orchestrator every raw transport's UploadFilesXXX wraps. initFn
// performs the one-shot manifest-and-materialize control request. newWorker
// is called once, with whether the source is a directory and whether the
// destination should be created as one, and must return the per-worker
// job-queue handler to pass to runChunkWorkers. numWorkers <= 0 uses the
// default (parallelStreamWorkers).
func uploadFiles(localPath, remotePath string, recursive bool, numWorkers int,
	initFn func(common.FSRequest) (*common.FSResponse, error),
	newWorker func(sourceIsDir, targetIsDirHint bool,
		progress *common.TransferProgress) func(jobs <-chan transferChunk) error,
) error {
	entries, err := common.BuildManifest(localPath, recursive)
	if err != nil {
		return err
	}

	sourceIsDir := entries[0].IsDir
	targetIsDirHint := strings.HasSuffix(remotePath, "/") ||
		filepath.Base(remotePath) == "." || filepath.Base(remotePath) == ".."

	if _, err := initFn(common.FSRequest{
		Op: common.FSOpInit, Path: remotePath, Entries: entries,
		SourceIsDir: sourceIsDir, TargetIsDirHint: targetIsDirHint,
	}); err != nil {
		return err
	}

	chunks := planChunks(entries)
	total := totalSize(entries)
	if total == 0 {
		return nil
	}

	progress := newTransferProgress(filepath.Base(localPath), total)
	err = runChunkWorkers(chunks, effectiveWorkers(numWorkers), newWorker(sourceIsDir, targetIsDirHint, progress))
	progress.Finish(err)
	return err
}
