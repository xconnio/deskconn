package common

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// FSOp identifies what a raw stream/channel request is asking the remote
// side to do. The same set of ops is used verbatim over both the WebRTC
// data-channel transport (filestreamchannel.go) and the QUIC stream
// transport (quictransfer.go). FSOpShell doesn't carry an FSRequest at all --
// it's only used in routingFrame.Op to route a raw QUIC stream to the shell
// handler instead of the file-transfer one (see HandleQUICStream).
type FSOp string

// FSRequest is the single request message sent on a fresh stream/channel.
// Which fields matter depends on Op: List needs Path/Recursive; Read needs
// Path/RelPath/Offset/Length; Init needs Path/Entries/SourceIsDir/
// TargetIsDirHint; Write needs Path/RelPath/Offset/Length/SourceIsDir/
// TargetIsDirHint (the latter two so the server can re-derive the same
// destination layout Init established, without keeping per-transfer state
// between the stateless per-chunk requests).
type FSRequest struct {
	Op              FSOp                    `json:"op"`
	Path            string                  `json:"path,omitempty"`
	Recursive       bool                    `json:"recursive,omitempty"`
	RelPath         string                  `json:"rel_path,omitempty"`
	Offset          int64                   `json:"offset,omitempty"`
	Length          int64                   `json:"length,omitempty"`
	Entries         []TransferManifestEntry `json:"entries,omitempty"`
	SourceIsDir     bool                    `json:"source_is_dir,omitempty"`
	TargetIsDirHint bool                    `json:"target_is_dir_hint,omitempty"`
}

// FSResponse is the single reply to an FSRequest. For Read it precedes the
// raw byte payload; for List it carries the manifest; Init/Write carry only
// OK/Err.
type FSResponse struct {
	OK      bool                    `json:"ok"`
	Err     string                  `json:"error,omitempty"`
	Entries []TransferManifestEntry `json:"entries,omitempty"`
}

// TransferManifestEntry describes one file or directory within a transfer,
// with RelPath rooted at the transfer's own top-level name (i.e. relative
// to the parent of the path the transfer was started on) -- the same
// convention fileHeaderMsg used for the WAMP-based transfer.
type TransferManifestEntry struct {
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	IsDir   bool   `json:"is_dir"`
}

// BuildManifest walks rootPath (a file, or if recursive a directory) and
// returns its entries relative to rootPath's own parent. rootPath is used
// as given -- callers resolve it against a remote home directory or the
// local cwd beforehand, whichever applies.
func BuildManifest(rootPath string, recursive bool) ([]TransferManifestEntry, error) {
	info, err := os.Lstat(rootPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() && !recursive {
		return nil, fmt.Errorf("%s: is a directory, use -r flag", rootPath)
	}

	basePath := filepath.Dir(rootPath)

	var entries []TransferManifestEntry
	var walk func(path string) error
	walk = func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relPath, _ := filepath.Rel(basePath, path)
		entries = append(entries, TransferManifestEntry{
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

// IsRootDir decides whether a transfer's destination root should be treated
// as a directory that entries nest under (true), or as the literal target
// path for the transfer's single top-level entry (false). sourceIsDir and
// targetIsDirHint let the caller force the directory interpretation before
// the destination exists (e.g. a recursive upload/download to a path that
// doesn't exist yet); otherwise it falls back to whatever is already there.
func IsRootDir(root string, sourceIsDir, targetIsDirHint bool) bool {
	if sourceIsDir || targetIsDirHint {
		return true
	}
	info, err := os.Lstat(root)
	return err == nil && info.IsDir()
}

// ResolveDestPath maps a manifest entry's RelPath onto a concrete path
// under root. If rootIsDir, entries nest under root by their full RelPath;
// otherwise the transfer's single top-level entry (RelPath == sourceRoot)
// is renamed to root itself, and any of its descendants nest under root by
// the remainder of their RelPath.
func ResolveDestPath(root string, rootIsDir bool, sourceRoot, relPath string) string {
	if rootIsDir {
		return filepath.Join(root, filepath.FromSlash(relPath))
	}
	suffix := strings.TrimPrefix(relPath, sourceRoot)
	return filepath.Clean(root + filepath.FromSlash(suffix))
}

// MaterializeTargets creates every directory and pre-sizes (creates and
// truncates to its final length) every file described by entries, rooted at
// root. Doing this once, up front, lets the parallel chunk workers open
// their destination file and WriteAt/copy into it without racing over
// creation or truncation.
func MaterializeTargets(entries []TransferManifestEntry, root string, rootIsDir bool) error {
	sourceRoot := ""
	if len(entries) > 0 {
		sourceRoot = entries[0].RelPath
	}

	for _, e := range entries {
		dest := ResolveDestPath(root, rootIsDir, sourceRoot, e.RelPath)
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

// RecvPriority reads one value from ch, preferring it over closed/timeout
// even if both are ready at the same instant -- select among multiple ready
// channels is otherwise pseudo-random, which could drop a value that was
// already sitting in ch's buffer when the peer closed its side right after
// sending it.
func RecvPriority[T any](ch <-chan T, closed <-chan struct{}, timeout time.Duration) (T, error) {
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

// TransferProgress aggregates byte counts reported by parallel workers into
// a single combined progress line, printed at most every progressPrintInterval.
type TransferProgress struct {
	Name      string
	Total     int64
	Start     time.Time
	Done      atomic.Int64
	LastPrint atomic.Int64 // UnixNano of the last printed update
}

func (p *TransferProgress) Add(n int64) {
	done := p.Done.Add(n)

	now := time.Now().UnixNano()
	last := p.LastPrint.Load()
	if now-last < progressPrintInterval {
		return
	}
	if !p.LastPrint.CompareAndSwap(last, now) {
		return // another goroutine just printed; don't fight over the line
	}
	PrintProgress(p.Name, done, p.Total, time.Since(p.Start))
}

// finish prints the closing progress line: the full total on success, or
// the actual bytes completed on failure rather than claiming 100%.
func (p *TransferProgress) Finish(err error) {
	done := p.Total
	if err != nil {
		done = p.Done.Load()
	}
	PrintProgress(p.Name, done, p.Total, time.Since(p.Start))
	fmt.Fprintln(os.Stderr)
}
