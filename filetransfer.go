package deskconn

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// parallelChunkSize is the byte range each parallel worker requests (or
	// pushes) from a single file before moving on to the next chunk in the
	// queue -- the unit of work for the worker pool, not the wire message
	// size (transports fragment it further as needed).
	parallelChunkSize = 4 * 1024 * 1024 // 4MB

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

// fsOp identifies what a raw stream/channel request is asking the remote
// side to do. The same set of ops is used verbatim over both the WebRTC
// data-channel transport (filestreamchannel.go) and the QUIC stream
// transport (quictransfer.go).
type fsOp string

const (
	fsOpList  fsOp = "list"  // enumerate a remote source (download manifest)
	fsOpRead  fsOp = "read"  // fetch one byte range of one file (download)
	fsOpInit  fsOp = "init"  // create dirs/pre-size files at a remote destination (upload)
	fsOpWrite fsOp = "write" // send one byte range of one file (upload)
)

// fsRequest is the single request message sent on a fresh stream/channel.
// Which fields matter depends on Op: List needs Path/Recursive; Read needs
// Path/RelPath/Offset/Length; Init needs Path/Entries/SourceIsDir/
// TargetIsDirHint; Write needs Path/RelPath/Offset/Length/SourceIsDir/
// TargetIsDirHint (the latter two so the server can re-derive the same
// destination layout Init established, without keeping per-transfer state
// between the stateless per-chunk requests).
type fsRequest struct {
	Op              fsOp                    `json:"op"`
	Path            string                  `json:"path,omitempty"`
	Recursive       bool                    `json:"recursive,omitempty"`
	RelPath         string                  `json:"rel_path,omitempty"`
	Offset          int64                   `json:"offset,omitempty"`
	Length          int64                   `json:"length,omitempty"`
	Entries         []transferManifestEntry `json:"entries,omitempty"`
	SourceIsDir     bool                    `json:"source_is_dir,omitempty"`
	TargetIsDirHint bool                    `json:"target_is_dir_hint,omitempty"`
}

// fsResponse is the single reply to an fsRequest. For Read it precedes the
// raw byte payload; for List it carries the manifest; Init/Write carry only
// OK/Err.
type fsResponse struct {
	OK      bool                    `json:"ok"`
	Err     string                  `json:"error,omitempty"`
	Entries []transferManifestEntry `json:"entries,omitempty"`
}

// transferManifestEntry describes one file or directory within a transfer,
// with RelPath rooted at the transfer's own top-level name (i.e. relative
// to the parent of the path the transfer was started on) -- the same
// convention fileHeaderMsg used for the WAMP-based transfer.
type transferManifestEntry struct {
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	IsDir   bool   `json:"is_dir"`
}

// transferChunk is one byte-range job in the flattened work queue the
// parallel workers draw from.
type transferChunk struct {
	RelPath string
	Offset  int64
	Length  int64
}

// buildManifest walks rootPath (a file, or if recursive a directory) and
// returns its entries relative to rootPath's own parent. rootPath is used
// as given -- callers resolve it against a remote home directory or the
// local cwd beforehand, whichever applies.
func buildManifest(rootPath string, recursive bool) ([]transferManifestEntry, error) {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() && !recursive {
		return nil, fmt.Errorf("%s: is a directory, use -r flag", rootPath)
	}

	basePath := filepath.Dir(rootPath)

	var entries []transferManifestEntry
	var walk func(path string) error
	walk = func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(basePath, path)
		entries = append(entries, transferManifestEntry{
			RelPath: filepath.ToSlash(relPath),
			Size:    info.Size(),
			Mode:    uint32(info.Mode().Perm()), //nolint:gosec
			IsDir:   info.IsDir(),
		})
		if !info.IsDir() {
			return nil
		}
		dirEntries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, e := range dirEntries {
			if err := walk(filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(rootPath); err != nil {
		return nil, err
	}
	return entries, nil
}

// planChunks flattens manifest file entries into byte-range jobs of at most
// parallelChunkSize each. Directories and zero-length files need no data
// transfer -- materializeTargets alone accounts for them.
func planChunks(entries []transferManifestEntry) []transferChunk {
	var chunks []transferChunk
	for _, e := range entries {
		if e.IsDir || e.Size == 0 {
			continue
		}
		for off := int64(0); off < e.Size; off += parallelChunkSize {
			length := int64(parallelChunkSize)
			if off+length > e.Size {
				length = e.Size - off
			}
			chunks = append(chunks, transferChunk{RelPath: e.RelPath, Offset: off, Length: length})
		}
	}
	return chunks
}

// totalSize sums the size of every non-directory entry.
func totalSize(entries []transferManifestEntry) int64 {
	var total int64
	for _, e := range entries {
		if !e.IsDir {
			total += e.Size
		}
	}
	return total
}

// isRootDir decides whether a transfer's destination root should be treated
// as a directory that entries nest under (true), or as the literal target
// path for the transfer's single top-level entry (false). sourceIsDir and
// targetIsDirHint let the caller force the directory interpretation before
// the destination exists (e.g. a recursive upload/download to a path that
// doesn't exist yet); otherwise it falls back to whatever is already there.
func isRootDir(root string, sourceIsDir, targetIsDirHint bool) bool {
	if sourceIsDir || targetIsDirHint {
		return true
	}
	info, err := os.Lstat(root)
	return err == nil && info.IsDir()
}

// resolveDestPath maps a manifest entry's RelPath onto a concrete path
// under root. If rootIsDir, entries nest under root by their full RelPath;
// otherwise the transfer's single top-level entry (RelPath == sourceRoot)
// is renamed to root itself, and any of its descendants nest under root by
// the remainder of their RelPath.
func resolveDestPath(root string, rootIsDir bool, sourceRoot, relPath string) string {
	if rootIsDir {
		return filepath.Join(root, filepath.FromSlash(relPath))
	}
	suffix := strings.TrimPrefix(relPath, sourceRoot)
	return filepath.Clean(root + filepath.FromSlash(suffix))
}

// materializeTargets creates every directory and pre-sizes (creates and
// truncates to its final length) every file described by entries, rooted at
// root. Doing this once, up front, lets the parallel chunk workers open
// their destination file and WriteAt/copy into it without racing over
// creation or truncation.
func materializeTargets(entries []transferManifestEntry, root string, rootIsDir bool) error {
	sourceRoot := ""
	if len(entries) > 0 {
		sourceRoot = entries[0].RelPath
	}

	for _, e := range entries {
		dest := resolveDestPath(root, rootIsDir, sourceRoot, e.RelPath)
		if e.IsDir {
			perm := os.FileMode(e.Mode)
			if perm == 0 {
				perm = 0755
			}
			if err := os.MkdirAll(dest, perm|0700); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil { //nolint:gosec
			return err
		}
		perm := os.FileMode(e.Mode)
		if perm == 0 {
			perm = 0600
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
		if err != nil && os.IsPermission(err) {
			if rmErr := os.Remove(dest); rmErr == nil {
				f, err = os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec
			}
		}
		if err != nil {
			return err
		}
		truncErr := f.Truncate(e.Size)
		closeErr := f.Close()
		if truncErr != nil {
			return truncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// runChunkWorkers starts up to workers goroutines, each running workerFn
// once against a shared jobs channel. workerFn is expected to open one
// connection (a raw stream/channel) up front and reuse it across every job
// it pulls off jobs, rather than opening a fresh one per chunk -- reopening
// per chunk was measured to badly limit throughput on real (non-loopback)
// links, since the connection never gets to reach steady-state flow control
// before it's torn down again. Stops feeding new jobs and returns the first
// error encountered; workers already mid-job are allowed to finish that job
// before exiting.
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
		SafeGo(func() {
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

// recvPriority reads one value from ch, preferring it over closed/timeout
// even if both are ready at the same instant -- select among multiple ready
// channels is otherwise pseudo-random, which could drop a value that was
// already sitting in ch's buffer when the peer closed its side right after
// sending it.
func recvPriority[T any](ch <-chan T, closed <-chan struct{}, timeout time.Duration) (T, error) {
	select {
	case v := <-ch:
		return v, nil
	default:
	}

	var zero T
	select {
	case v := <-ch:
		return v, nil
	case <-closed:
		return zero, io.ErrClosedPipe
	case <-time.After(timeout):
		return zero, fmt.Errorf("timed out waiting for response")
	}
}

// progressPrintInterval caps how often transferProgress.add actually prints
// an update. Without this, a fine-grained transport -- e.g. one call per
// 16KB WebRTC message, tens of thousands of times a second for a large file
// -- pays for a fmt.Fprintf on every single one, on the same goroutine
// that's reading data off the wire. If stderr is at all slow to drain (a
// redirected terminal, tmux, an SSH session), that print stalls the read
// loop feeding it and throttles real transfer throughput down to however
// fast the terminal can redraw a line, not how fast the network can go.
const progressPrintInterval = int64(100 * time.Millisecond)

// transferProgress aggregates byte counts reported by parallel workers into
// a single combined progress line, printed at most every progressPrintInterval.
type transferProgress struct {
	name      string
	total     int64
	start     time.Time
	done      atomic.Int64
	lastPrint atomic.Int64 // UnixNano of the last printed update
}

func newTransferProgress(name string, total int64) *transferProgress {
	p := &transferProgress{name: name, total: total, start: time.Now()}
	p.lastPrint.Store(p.start.UnixNano())
	printProgress(p.name, 0, p.total, 0)
	return p
}

func (p *transferProgress) add(n int64) {
	done := p.done.Add(n)

	now := time.Now().UnixNano()
	last := p.lastPrint.Load()
	if now-last < progressPrintInterval {
		return
	}
	if !p.lastPrint.CompareAndSwap(last, now) {
		return // another goroutine just printed; don't fight over the line
	}
	printProgress(p.name, done, p.total, time.Since(p.start))
}

// finish prints the closing progress line. On success it shows the full
// total (100%); on failure it shows the actual bytes completed instead of
// lying about having finished -- runChunkWorkers stops feeding new jobs on
// the first error, so a failed transfer is genuinely incomplete, and the
// progress line should say so rather than claim 100%. The error itself is
// left to the caller to report (it's already doing that), so this only
// adjusts what byte count gets displayed.
func (p *transferProgress) finish(err error) {
	done := p.total
	if err != nil {
		done = p.done.Load()
	}
	printProgress(p.name, done, p.total, time.Since(p.start))
	fmt.Fprintln(os.Stderr)
}

// downloadFiles is the single reusable download orchestrator every raw
// transport's PullFilesXXX wraps (filetransferp2p.go's DownloadFilesP2P,
// quictransfer.go's DownloadFilesQUIC): list the remote source via listFn,
// pre-create local targets, plan chunks, and run numWorkers parallel
// workers built by newWorker -- mirroring how the old WAMP-based transfer
// had one reusable pullFilesInternal behind PullFiles/PullFilesViaProxy.
// numWorkers <= 0 uses the default (parallelStreamWorkers).
//
// listFn performs the one-shot manifest request. newWorker is called once,
// with the manifest's source root and whether localPath should be treated
// as a directory, and must return the per-worker job-queue handler to pass
// to runChunkWorkers.
func downloadFiles(remotePath, localPath string, recursive bool, numWorkers int,
	listFn func(fsRequest) (*fsResponse, error),
	newWorker func(sourceRoot string, localIsDir bool, progress *transferProgress) func(jobs <-chan transferChunk) error,
) error {
	resp, err := listFn(fsRequest{Op: fsOpList, Path: remotePath, Recursive: recursive})
	if err != nil {
		return err
	}
	entries := resp.Entries
	if len(entries) == 0 {
		return fmt.Errorf("%s: no such file or directory", remotePath)
	}

	localIsDir := isRootDir(localPath, entries[0].IsDir, false)
	if err := materializeTargets(entries, localPath, localIsDir); err != nil {
		return err
	}

	chunks := planChunks(entries)
	total := totalSize(entries)
	if total == 0 {
		return nil
	}

	progress := newTransferProgress(filepath.Base(remotePath), total)
	err = runChunkWorkers(chunks, effectiveWorkers(numWorkers), newWorker(entries[0].RelPath, localIsDir, progress))
	progress.finish(err)
	return err
}

// uploadFiles is the upload counterpart to downloadFiles: the single reusable
// orchestrator every raw transport's PushFilesXXX wraps. initFn performs
// the one-shot manifest-and-materialize control request. newWorker is
// called once, with whether the source is a directory and whether the
// destination should be created as one, and must return the per-worker
// job-queue handler to pass to runChunkWorkers. numWorkers <= 0 uses the
// default (parallelStreamWorkers).
func uploadFiles(localPath, remotePath string, recursive bool, numWorkers int,
	initFn func(fsRequest) (*fsResponse, error),
	newWorker func(sourceIsDir, targetIsDirHint bool, progress *transferProgress) func(jobs <-chan transferChunk) error,
) error {
	entries, err := buildManifest(localPath, recursive)
	if err != nil {
		return err
	}

	sourceIsDir := entries[0].IsDir
	targetIsDirHint := strings.HasSuffix(remotePath, "/") ||
		filepath.Base(remotePath) == "." || filepath.Base(remotePath) == ".."

	if _, err := initFn(fsRequest{
		Op: fsOpInit, Path: remotePath, Entries: entries,
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
	progress.finish(err)
	return err
}
