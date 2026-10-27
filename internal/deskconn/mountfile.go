//go:build linux

package deskconn

import (
	"bytes"
	"context"
	"sync"
	"syscall"
	"time"

	"github.com/xconnio/deskconn/internal/common"
)

// The kernel reads a FUSE file sequentially in pieces of at most 256 KiB, and
// writes it in pieces of at most 1 MiB, each only once the last one is done. Over
// a link with a long round trip (routed mode goes through the cloud router) that
// would wait out one round trip per piece. So mountFile fetches a file being read
// sequentially in mountBlockSize blocks, keeping the next mountReadAhead of them
// in flight, and acknowledges writes before the machine does, keeping up to
// mountWriteBehind of them in flight; their errors are reported by the next
// write, flush (close) or fsync, as NFS does.
const (
	mountBlockSize   = common.MountMaxIO
	mountReadAhead   = 4
	mountWriteBehind = 4
)

type mountBlock struct {
	done    chan struct{}
	data    []byte
	errno   syscall.Errno
	fetched time.Time
}

type mountWrite struct {
	off, end int64
}

// mountFile is a file open on the machine (common.MountOpOpen).
type mountFile struct {
	handle uint64
	mfs    *mountFS
	// ctx ends when the file is released, cancelling fetches still in flight.
	ctx    context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// size is the file's size when opened; nothing is fetched ahead beyond it.
	size   int64
	blocks map[int64]*mountBlock
	// next is where a sequential reader would read next.
	next int64

	wmu     sync.Mutex
	wdone   *sync.Cond
	writing []*mountWrite
	// werr is the first write that failed after it was acknowledged.
	werr syscall.Errno
}

func newMountFile(mfs *mountFS, handle uint64, size int64) *mountFile {
	ctx, cancel := context.WithCancel(context.Background())
	f := &mountFile{
		handle: handle,
		mfs:    mfs,
		ctx:    ctx,
		cancel: cancel,
		size:   size,
		blocks: make(map[int64]*mountBlock),
	}
	f.wdone = sync.NewCond(&f.wmu)
	return f
}

// write sends data to the machine at off, returning before it's written.
func (f *mountFile) write(data []byte, off int64) (uint32, syscall.Errno) {
	w := &mountWrite{off: off, end: off + int64(len(data))}

	f.wmu.Lock()
	// Writes to the same bytes must reach the machine in order.
	for f.werr == 0 && (len(f.writing) >= mountWriteBehind || f.overlapsLocked(w)) {
		f.wdone.Wait()
	}
	if f.werr != 0 {
		f.wmu.Unlock()
		return 0, f.werr
	}
	f.writing = append(f.writing, w)
	f.wmu.Unlock()
	f.invalidate(w.end)

	// The kernel reuses data's buffer once this returns.
	data = bytes.Clone(data)
	common.SafeGo(func() {
		_, errno := f.mfs.mutate(f.ctx, &common.MountRequest{
			Op: common.MountOpWrite, Handle: f.handle, Offset: off, Data: data,
		})
		f.invalidate(w.end)
		f.wmu.Lock()
		if errno != 0 && f.werr == 0 {
			f.werr = errno
		}
		for i, pending := range f.writing {
			if pending == w {
				f.writing = append(f.writing[:i], f.writing[i+1:]...)
				break
			}
		}
		f.wdone.Broadcast()
		f.wmu.Unlock()
	})
	return uint32(len(data)), 0 // nolint: gosec
}

func (f *mountFile) overlapsLocked(w *mountWrite) bool {
	for _, pending := range f.writing {
		if pending.off < w.end && w.off < pending.end {
			return true
		}
	}
	return false
}

// flush waits for f's writes to reach the machine, returning whether any were
// pending and the first that failed.
func (f *mountFile) flush() (bool, syscall.Errno) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	pending := len(f.writing) > 0
	for len(f.writing) > 0 {
		f.wdone.Wait()
	}
	return pending, f.werr
}

func (f *mountFile) fsync(ctx context.Context) syscall.Errno {
	if _, errno := f.flush(); errno != 0 {
		return errno
	}
	_, errno := f.mfs.do(ctx, &common.MountRequest{Op: common.MountOpFsync, Handle: f.handle})
	return errno
}

// read reads into dest at off, from blocks while the file is read sequentially.
func (f *mountFile) read(ctx context.Context, dest []byte, off int64) ([]byte, syscall.Errno) {
	end := off + int64(len(dest))

	f.mu.Lock()
	// The kernel may send a few reads of a sequential stream at once, so out of
	// order: anything near where the stream is counts as part of it.
	sequential := off >= f.next-2*mountBlockSize && off <= f.next+2*mountBlockSize
	f.next = max(f.next, end)
	if !sequential {
		f.next = end
	}

	first, last := off/mountBlockSize, (end-1)/mountBlockSize
	cached := true
	for i := first; i <= last; i++ {
		if !f.freshLocked(i) {
			cached = false
		}
	}
	if !sequential && !cached {
		f.mu.Unlock()
		return f.readDirect(ctx, dest, off)
	}

	for i := range f.blocks {
		if i < first-1 {
			delete(f.blocks, i)
		}
	}
	for i := first; i <= last; i++ {
		f.fetchLocked(i)
	}
	if sequential {
		for i := last + 1; i <= last+mountReadAhead && i*mountBlockSize < f.size; i++ {
			f.fetchLocked(i)
		}
	}
	blocks := make([]*mountBlock, 0, last-first+1)
	for i := first; i <= last; i++ {
		blocks = append(blocks, f.blocks[i])
	}
	f.mu.Unlock()

	n := 0
	for i, b := range blocks {
		select {
		case <-b.done:
		case <-ctx.Done():
			return nil, syscall.EINTR
		}
		if b.errno != 0 {
			f.mu.Lock()
			if f.blocks[first+int64(i)] == b {
				delete(f.blocks, first+int64(i))
			}
			f.mu.Unlock()
			return nil, b.errno
		}
		start := off + int64(n) - (first+int64(i))*mountBlockSize
		if start >= int64(len(b.data)) {
			break
		}
		n += copy(dest[n:], b.data[start:])
		if len(b.data) < mountBlockSize {
			break // end of file
		}
	}
	return dest[:n], 0
}

// freshLocked reports whether block i is cached (or on its way) and recent
// enough to use.
func (f *mountFile) freshLocked(i int64) bool {
	b, ok := f.blocks[i]
	if !ok {
		return false
	}
	select {
	case <-b.done:
		return b.errno == 0 && time.Since(b.fetched) < mountCacheTimeout
	default:
		return true
	}
}

// fetchLocked starts fetching block i unless a fresh copy is cached or coming.
func (f *mountFile) fetchLocked(i int64) {
	if f.freshLocked(i) {
		return
	}
	b := &mountBlock{done: make(chan struct{})}
	f.blocks[i] = b
	common.SafeGo(func() {
		defer close(b.done)
		resp, errno := f.mfs.do(f.ctx, &common.MountRequest{
			Op: common.MountOpRead, Handle: f.handle, Offset: i * mountBlockSize, Length: mountBlockSize,
		})
		b.fetched = time.Now()
		if errno != 0 {
			b.errno = errno
			return
		}
		b.data = resp.Data
	})
}

func (f *mountFile) readDirect(ctx context.Context, dest []byte, off int64) ([]byte, syscall.Errno) {
	resp, errno := f.mfs.do(ctx, &common.MountRequest{
		Op: common.MountOpRead, Handle: f.handle, Offset: off, Length: min(len(dest), common.MountMaxIO),
	})
	if errno != 0 {
		return nil, errno
	}
	return resp.Data, 0
}

// invalidate drops the cached blocks, after the file is written through f.
func (f *mountFile) invalidate(end int64) {
	f.mu.Lock()
	clear(f.blocks)
	f.size = max(f.size, end)
	f.mu.Unlock()
}

func (f *mountFile) release(ctx context.Context) syscall.Errno {
	_, _ = f.flush()
	f.cancel()
	_, errno := f.mfs.do(ctx, &common.MountRequest{Op: common.MountOpRelease, Handle: f.handle})
	return errno
}
