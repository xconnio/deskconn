//go:build linux

package deskconn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/xconnio/deskconn/internal/common"
	"github.com/xconnio/xconn-core/bindings/go"
)

// mountCacheTimeout is how long the kernel (and mountFS's own readdir
// cache) may trust attributes and lookups before asking the machine again.
// Short, because the machine's files can change underneath the mount.
const mountCacheTimeout = time.Second

// mountRelistWindow is how long after listing a directory a lookup in it that
// misses the cache lists it again rather than looking up just that entry.
const mountRelistWindow = 10 * time.Second

// mountCallTimeout is how long a request may take. The router drops messages
// for a peer that's too far behind, so a reply may never come; without a limit,
// whatever was waiting for it (an ls, a cp) would hang, unkillable.
const mountCallTimeout = time.Minute

type mountCallResult struct {
	result []byte
	err    error
}

type mountListed struct {
	at    time.Time
	names map[string]struct{}
}

type mountListing struct {
	done    chan struct{}
	entries []common.MountDirEntry
	errno   syscall.Errno
}

// MountCaller sends one common.MountRequest, framed by
// common.EncodeMountMessage, to the machine and returns the framed
// common.MountResponse.
type MountCaller func(ctx context.Context, payload []byte) ([]byte, error)

// ProxyMountCaller routes mount requests through the local daemon's persistent
// machine session, like the other `file` commands' default mode.
func ProxyMountCaller(localSession *xconn.Session, realm string) MountCaller {
	return func(ctx context.Context, payload []byte) ([]byte, error) {
		resp := localSession.Call(common.ProcedureProxyFileOp).
			Args(realm, common.ProcedureMountFS, payload).ExecuteContext(ctx)
		if resp.Err != nil {
			return nil, resp.Err
		}
		return resp.ArgBytes(0)
	}
}

// DirectMountCaller sends mount requests over a session to the machine itself
// (--mode routed/direct), exchanging keys once and again only if the machine
// has dropped them.
func DirectMountCaller(deviceSession *xconn.Session) MountCaller {
	var (
		mu  sync.Mutex
		enc *common.SessionKeys
	)
	keys := func(renew bool) (*common.SessionKeys, error) {
		mu.Lock()
		defer mu.Unlock()
		if enc == nil || renew {
			newEnc, err := common.ClientKeyExchange(deviceSession)
			if err != nil {
				return nil, err
			}
			enc = newEnc
		}
		return enc, nil
	}

	return func(_ context.Context, payload []byte) ([]byte, error) {
		renew := false
		for {
			k, err := keys(renew)
			if err != nil {
				return nil, err
			}
			result, err := common.EncryptedCall(deviceSession, common.ProcedureMountFS, payload, k)
			if err != nil && !renew && strings.Contains(err.Error(), "no session keys found") {
				renew = true
				continue
			}
			return result, err
		}
	}
}

// MountOptions are how Mount mounts.
type MountOptions struct {
	ReadOnly bool
	Debug    bool
}

// Mount mounts remoteRoot (a path on the machine, "" for its home
// directory) at mountpoint and serves it until it's unmounted, either
// externally (fusermount -u) or by SIGINT/SIGTERM.
func Mount(call MountCaller, remoteRoot, mountpoint, fsName string, opts MountOptions) error {
	server, err := MountServer(call, remoteRoot, mountpoint, fsName, opts)
	if err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	done := make(chan struct{})
	common.SafeGo(func() {
		for {
			select {
			case <-sigCh:
				if err := server.Unmount(); err != nil {
					fmt.Fprintf(os.Stderr, "unmount %s: %v (close files using it, or run `fusermount -u %s`)\n",
						mountpoint, err, mountpoint)
				}
			case <-done:
				return
			}
		}
	})

	fmt.Fprintf(os.Stderr, "mounted %s at %s, press Ctrl-C to unmount\n", fsName, mountpoint)
	server.Wait()
	close(done)
	return nil
}

// MountServer mounts remoteRoot at mountpoint and returns the server
// serving it, without waiting for it to be unmounted.
func MountServer(call MountCaller, remoteRoot, mountpoint, fsName string, opts MountOptions) (*fuse.Server, error) {
	if mounted, err := isMountpoint(mountpoint); err == nil && mounted {
		// fusermount only says it can't access it.
		return nil, fmt.Errorf("%s is already mounted; unmount it first with `fusermount -u %s`", mountpoint, mountpoint)
	}

	mfs := &mountFS{
		call:    call,
		root:    remoteRoot,
		uid:     uint32(os.Getuid()), // nolint: gosec
		gid:     uint32(os.Getgid()), // nolint: gosec
		cache:   make(map[string]mountCacheEntry),
		listed:  make(map[string]*mountListed),
		listing: make(map[string]*mountListing),
		inos:    make(map[[2]uint64]uint64),
		// 1 is the root's.
		nextIno:   1,
		transfers: make(chan struct{}, mountInFlight),
	}

	resp, errno := mfs.do(context.Background(), &common.MountRequest{
		Op: common.MountOpStat, Path: remoteRoot, Follow: true,
	})
	if errno != 0 {
		return nil, fmt.Errorf("%s: %w", displayRoot(remoteRoot), errno)
	}
	if resp.Attr.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return nil, fmt.Errorf("%s: %w", displayRoot(remoteRoot), syscall.ENOTDIR)
	}

	timeout := mountCacheTimeout
	mountOpts := fuse.MountOptions{
		FsName:       fsName,
		Name:         "deskconn",
		MaxWrite:     mountBlockSize,
		MaxReadAhead: mountBlockSize,
		Debug:        opts.Debug,
	}
	if opts.ReadOnly {
		mountOpts.Options = append(mountOpts.Options, "ro")
	}

	server, err := fs.Mount(mountpoint, &mountNode{mfs: mfs}, &fs.Options{
		MountOptions:    mountOpts,
		EntryTimeout:    &timeout,
		AttrTimeout:     &timeout,
		NegativeTimeout: &timeout,
		UID:             mfs.uid,
		GID:             mfs.gid,
	})
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w", mountpoint, err)
	}
	return server, nil
}

// isMountpoint reports whether something is mounted at dir.
func isMountpoint(dir string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(string(data)) {
		// The mount point is the fifth field, with spaces and the like escaped
		// as octal.
		fields := strings.Fields(line)
		if len(fields) > 4 && unescapeMountinfo(fields[4]) == abs {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func displayRoot(remoteRoot string) string {
	if remoteRoot == "" {
		return "~"
	}
	return remoteRoot
}

type mountCacheEntry struct {
	attr    common.MountAttr
	expires time.Time
}

type mountFS struct {
	call     MountCaller
	root     string
	uid, gid uint32

	// cache holds the attributes the last readdir returned, so the
	// lookup the kernel sends for every entry of an `ls -l` doesn't cost
	// another round trip to the machine each.
	mu    sync.Mutex
	cache map[string]mountCacheEntry
	// listed is when each directory was last listed; listing are the
	// listings in flight.
	listed  map[string]*mountListed
	listing map[string]*mountListing

	// inos are the inode numbers handed to the kernel, by the machine's (dev,
	// ino). They must stay the same for a file: tar, git and find compare them.
	inoMu   sync.Mutex
	inos    map[[2]uint64]uint64
	nextIno uint64

	// transfers holds a slot for each request carrying file data in flight.
	transfers chan struct{}
}

// acquire waits for a slot for a request carrying file data; release frees it.
func (m *mountFS) acquire() {
	m.transfers <- struct{}{}
}

func (m *mountFS) release() {
	<-m.transfers
}

// transfer sends a request carrying file data: a read or write of at most
// mountBlockSize, at most mountInFlight of them at once.
func (m *mountFS) transfer(ctx context.Context, req *common.MountRequest) (*common.MountResponse, syscall.Errno) {
	m.acquire()
	defer m.release()
	if req.Op == common.MountOpWrite {
		return m.mutate(ctx, req)
	}
	return m.do(ctx, req)
}

func (m *mountFS) do(ctx context.Context, req *common.MountRequest) (*common.MountResponse, syscall.Errno) {
	payload, err := common.EncodeMountMessage(req, req.Data)
	if err != nil {
		return nil, syscall.EINVAL
	}
	result, err := m.callAbandoning(ctx, payload)
	if err != nil {
		log.Printf("mount: %s %s: %v", req.Op, req.Path, err)
		return nil, syscall.EIO
	}
	var resp common.MountResponse
	data, err := common.DecodeMountMessage(result, &resp)
	if err != nil {
		log.Printf("mount: %s %s: invalid response: %v", req.Op, req.Path, err)
		return nil, syscall.EIO
	}
	if resp.Errno != "" {
		return nil, common.MountErrno(resp.Errno)
	}
	resp.Data = data
	return &resp, 0
}

// callAbandoning sends payload, giving up on the reply once mountCallTimeout
// has passed. It abandons the call rather than cancel it: a router answers the
// cancel of a call it has just finished with an error that xconn-go's session
// can't place, which stops it reading anything more.
//
// Nor does it give up when ctx is done, which for a request from the kernel
// means the process making it got a signal. Programs don't expect a file
// system call to fail with EINTR on one they handle (git, whose progress
// display runs on SIGALRM, gives up on the file), and the kernel stops waiting
// by itself on one that kills the process.
func (m *mountFS) callAbandoning(ctx context.Context, payload []byte) ([]byte, error) {
	done := make(chan mountCallResult, 1)
	common.SafeGo(func() {
		result, err := m.call(context.WithoutCancel(ctx), payload)
		done <- mountCallResult{result: result, err: err}
	})
	timer := time.NewTimer(mountCallTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.result, r.err
	case <-timer.C:
		return nil, errors.New("no reply")
	}
}

// forget drops p's cached attributes.
func (m *mountFS) forget(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cache, p)
}

// mutate runs an operation that changes the machine's files, dropping
// everything cached since it may now be stale.
func (m *mountFS) mutate(ctx context.Context, req *common.MountRequest) (*common.MountResponse, syscall.Errno) {
	m.mu.Lock()
	clear(m.cache)
	m.mu.Unlock()
	return m.do(ctx, req)
}

// stat returns p's attributes, of what it points to if it's a symlink and
// follow is set.
func (m *mountFS) stat(ctx context.Context, p string, follow bool) (*common.MountAttr, syscall.Errno) {
	if attr, ok := m.cached(p); ok {
		return attr, 0
	}

	// Listing a directory (ls -l, find, a file manager) looks up each of its
	// entries next. Over a slow link the listing's attributes may expire
	// before it gets through them, each lookup then taking a round trip of
	// its own; listing the directory again refreshes them all in one.
	dir, name := path.Split(p)
	dir = path.Clean(dir)
	if m.listedRecently(dir, name) {
		if _, errno := m.list(ctx, dir); errno == 0 {
			if attr, ok := m.cached(p); ok {
				return attr, 0
			}
		}
	}

	resp, errno := m.do(ctx, &common.MountRequest{Op: common.MountOpStat, Path: p, Follow: follow})
	if errno != 0 {
		return nil, errno
	}
	return resp.Attr, 0
}

func (m *mountFS) cached(p string) (*common.MountAttr, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.cache[p]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(m.cache, p)
		return nil, false
	}
	return &entry.attr, true
}

// listedRecently reports whether dir was listed recently, with name in it.
// Names that weren't (files being created) are looked up on their own: a
// listing, possibly of a large directory, wouldn't have them.
func (m *mountFS) listedRecently(dir, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	listed, ok := m.listed[dir]
	if !ok || time.Since(listed.at) >= mountRelistWindow {
		return false
	}
	_, ok = listed.names[name]
	return ok
}

// list lists dir on the machine, caching its own and its entries' attributes.
// Concurrent lists of the same directory share one request.
func (m *mountFS) list(ctx context.Context, dir string) ([]common.MountDirEntry, syscall.Errno) {
	m.mu.Lock()
	if inFlight, ok := m.listing[dir]; ok {
		m.mu.Unlock()
		<-inFlight.done
		return inFlight.entries, inFlight.errno
	}
	l := &mountListing{done: make(chan struct{})}
	m.listing[dir] = l
	m.mu.Unlock()

	resp, errno := m.do(ctx, &common.MountRequest{Op: common.MountOpReadDir, Path: dir})

	m.mu.Lock()
	delete(m.listing, dir)
	if errno == 0 {
		l.entries = resp.Entries
		expires := time.Now().Add(mountCacheTimeout)
		if resp.Attr != nil {
			m.cache[dir] = mountCacheEntry{attr: *resp.Attr, expires: expires}
		}
		for _, entry := range resp.Entries {
			m.cache[path.Join(dir, entry.Name)] = mountCacheEntry{attr: entry.Attr, expires: expires}
		}
		names := make(map[string]struct{}, len(resp.Entries))
		for _, entry := range resp.Entries {
			names[entry.Name] = struct{}{}
		}
		m.listed[dir] = &mountListed{at: time.Now(), names: names}
	}
	l.errno = errno
	m.mu.Unlock()
	close(l.done)
	return l.entries, errno
}

// ino is the inode number for a's file, or 0 for go-fuse to pick one if the
// machine doesn't identify its files.
func (m *mountFS) ino(a *common.MountAttr) uint64 {
	if a.Ino == 0 {
		return 0
	}
	key := [2]uint64{a.Dev, a.Ino}
	m.inoMu.Lock()
	defer m.inoMu.Unlock()
	ino, ok := m.inos[key]
	if !ok {
		m.nextIno++
		ino = m.nextIno
		m.inos[key] = ino
	}
	return ino
}

func (m *mountFS) fillAttr(a *common.MountAttr, out *fuse.Attr) {
	out.Mode = a.Mode
	out.Size = uint64(a.Size) // nolint: gosec
	out.Blocks = (out.Size + 511) / 512
	out.Blksize = 4096
	out.Nlink = a.Nlink
	out.Owner = fuse.Owner{Uid: m.uid, Gid: m.gid}
	atime, mtime, ctime := time.Unix(0, a.Atime), time.Unix(0, a.Mtime), time.Unix(0, a.Ctime)
	out.SetTimes(&atime, &mtime, &ctime)
}

// mountNode is a file, directory or symlink of the mount, identified by
// its path from the mount root (the machine's paths aren't stable across
// renames anyway, so there's nothing better to key it by).
type mountNode struct {
	fs.Inode
	mfs *mountFS

	mu    sync.Mutex
	files map[*mountFile]struct{}
}

func (n *mountNode) addFile(f *mountFile) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.files == nil {
		n.files = make(map[*mountFile]struct{})
	}
	n.files[f] = struct{}{}
}

func (n *mountNode) removeFile(f *mountFile) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.files, f)
}

func (n *mountNode) openFiles() []*mountFile {
	n.mu.Lock()
	defer n.mu.Unlock()
	files := make([]*mountFile, 0, len(n.files))
	for f := range n.files {
		files = append(files, f)
	}
	return files
}

// flushWrites waits for the writes n's open files have acknowledged, so the
// machine's contents include them, dropping n's cached attributes if there were
// any.
func (n *mountNode) flushWrites() {
	flushed := false
	for _, f := range n.openFiles() {
		if pending, _ := f.flush(); pending {
			flushed = true
		}
	}
	if flushed {
		n.mfs.forget(n.remotePath())
	}
}

var (
	_ fs.NodeGetattrer  = (*mountNode)(nil)
	_ fs.NodeSetattrer  = (*mountNode)(nil)
	_ fs.NodeLookuper   = (*mountNode)(nil)
	_ fs.NodeReaddirer  = (*mountNode)(nil)
	_ fs.NodeOpener     = (*mountNode)(nil)
	_ fs.NodeReader     = (*mountNode)(nil)
	_ fs.NodeWriter     = (*mountNode)(nil)
	_ fs.NodeFsyncer    = (*mountNode)(nil)
	_ fs.NodeFlusher    = (*mountNode)(nil)
	_ fs.NodeReleaser   = (*mountNode)(nil)
	_ fs.NodeCreater    = (*mountNode)(nil)
	_ fs.NodeMkdirer    = (*mountNode)(nil)
	_ fs.NodeUnlinker   = (*mountNode)(nil)
	_ fs.NodeRmdirer    = (*mountNode)(nil)
	_ fs.NodeRenamer    = (*mountNode)(nil)
	_ fs.NodeSymlinker  = (*mountNode)(nil)
	_ fs.NodeReadlinker = (*mountNode)(nil)
	_ fs.NodeStatfser   = (*mountNode)(nil)
)

// remotePath is n's path on the machine, "." for the home directory: the same
// form path.Dir and path.Join give, so it also keys mountFS's caches.
func (n *mountNode) remotePath() string {
	if p := path.Join(n.mfs.root, n.Path(n.Root())); p != "" {
		return p
	}
	return "."
}

func (n *mountNode) childPath(name string) string {
	return path.Join(n.remotePath(), name)
}

func (n *mountNode) newChild(ctx context.Context, attr *common.MountAttr, out *fuse.EntryOut) *fs.Inode {
	n.mfs.fillAttr(attr, &out.Attr)
	stable := fs.StableAttr{Mode: attr.Mode & syscall.S_IFMT, Ino: n.mfs.ino(attr)}
	return n.NewInode(ctx, &mountNode{mfs: n.mfs}, stable)
}

// withWrites is attr with the writes n's open files have acknowledged, rather
// than waiting for them to reach the machine (which a file being copied in
// would make an ls wait out).
func (n *mountNode) withWrites(attr *common.MountAttr) *common.MountAttr {
	for _, f := range n.openFiles() {
		if size := f.writtenSize(); size > attr.Size {
			withSize := *attr
			withSize.Size = size
			attr = &withSize
		}
	}
	return attr
}

// truncated tells n's open files it was truncated (or extended) to size.
func (n *mountNode) truncated(size int64) {
	for _, f := range n.openFiles() {
		f.truncated(size)
	}
}

func (n *mountNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// The mount's root may be a symlink to a directory on the machine.
	attr, errno := n.mfs.stat(ctx, n.remotePath(), n.IsRoot())
	if errno != 0 {
		return errno
	}
	n.mfs.fillAttr(n.withWrites(attr), &out.Attr)
	return 0
}

func (n *mountNode) Setattr(ctx context.Context, _ fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.flushWrites()
	req := &common.MountRequest{Op: common.MountOpChange, Path: n.remotePath()}
	if size, ok := in.GetSize(); ok {
		s := int64(size) // nolint: gosec
		req.NewSize = &s
	}
	if mode, ok := in.GetMode(); ok {
		req.NewMode = &mode
	}
	if atime, ok := in.GetATime(); ok {
		t := atime.UnixNano()
		req.NewAtime = &t
	}
	if mtime, ok := in.GetMTime(); ok {
		t := mtime.UnixNano()
		req.NewMtime = &t
	}
	// Ownership changes are ignored: the machine's files all belong to the
	// user the daemon runs as, and are shown as the mounting user's.

	resp, errno := n.mfs.mutate(ctx, req)
	if errno != 0 {
		return errno
	}
	if req.NewSize != nil {
		n.truncated(*req.NewSize)
	}
	n.mfs.fillAttr(resp.Attr, &out.Attr)
	return 0
}

func (n *mountNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	attr, errno := n.mfs.stat(ctx, n.childPath(name), false)
	if errno != 0 {
		return nil, errno
	}
	if child := n.GetChild(name); child != nil {
		if childNode, ok := child.Operations().(*mountNode); ok {
			attr = childNode.withWrites(attr)
		}
	}
	return n.newChild(ctx, attr, out), 0
}

func (n *mountNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	listing, errno := n.mfs.list(ctx, n.remotePath())
	if errno != 0 {
		return nil, errno
	}

	entries := make([]fuse.DirEntry, 0, len(listing))
	for _, entry := range listing {
		entries = append(entries, fuse.DirEntry{Name: entry.Name, Mode: entry.Attr.Mode, Ino: n.mfs.ino(&entry.Attr)})
	}
	return fs.NewListDirStream(entries), 0
}

// open opens p on the machine with flags' access mode, creating it with mode
// when create is set.
func (n *mountNode) open(ctx context.Context, p string, flags, mode uint32,
	create bool) (*mountFile, *common.MountAttr, syscall.Errno) {
	req := &common.MountRequest{
		Op:        common.MountOpOpen,
		Path:      p,
		Mode:      mode,
		Create:    create,
		Exclusive: flags&syscall.O_EXCL != 0,
		Truncate:  flags&syscall.O_TRUNC != 0,
	}
	switch flags & syscall.O_ACCMODE {
	case syscall.O_WRONLY:
		req.Access = common.MountAccessWrite
	case syscall.O_RDWR:
		req.Access = common.MountAccessReadWrite
	default:
		req.Access = common.MountAccessRead
	}

	do := n.mfs.do
	if create || req.Truncate {
		do = n.mfs.mutate
	}
	resp, errno := do(ctx, req)
	if errno != 0 {
		return nil, nil, errno
	}
	return newMountFile(n.mfs, resp.Handle, resp.Attr.Size), resp.Attr, 0
}

func (n *mountNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	f, _, errno := n.open(ctx, n.remotePath(), flags, 0, false)
	if errno != 0 {
		return nil, 0, errno
	}
	if flags&syscall.O_TRUNC != 0 {
		n.truncated(0)
	}
	n.addFile(f)
	return f, 0, 0
}

func (n *mountNode) Create(ctx context.Context, name string, flags, mode uint32,
	out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	f, attr, errno := n.open(ctx, n.childPath(name), flags, mode, true)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	child := n.newChild(ctx, attr, out)
	child.Operations().(*mountNode).addFile(f)
	return child, f, 0, 0
}

func (n *mountNode) Release(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	f, ok := fh.(*mountFile)
	if !ok {
		return 0
	}
	n.removeFile(f)
	// The kernel doesn't wait for (or retry) a release, so don't let an
	// interrupted request context leak the machine's file.
	return f.release(context.WithoutCancel(ctx))
}

func (n *mountNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte,
	off int64) (fuse.ReadResult, syscall.Errno) {
	n.flushWrites()
	if f, ok := fh.(*mountFile); ok {
		data, errno := f.read(ctx, dest, off)
		if errno != 0 {
			return nil, errno
		}
		return fuse.ReadResultData(data), 0
	}

	resp, errno := n.mfs.transfer(ctx, &common.MountRequest{
		Op: common.MountOpRead, Path: n.remotePath(), Offset: off, Length: min(len(dest), mountBlockSize),
	})
	if errno != 0 {
		return nil, errno
	}
	return fuse.ReadResultData(resp.Data), 0
}

func (n *mountNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	if f, ok := fh.(*mountFile); ok {
		return f.write(data, off)
	}

	resp, errno := n.mfs.transfer(ctx, &common.MountRequest{
		Op: common.MountOpWrite, Path: n.remotePath(), Offset: off, Data: data,
	})
	if errno != 0 {
		return 0, errno
	}
	return uint32(resp.Written), 0 // nolint: gosec
}

func (n *mountNode) Flush(_ context.Context, fh fs.FileHandle) syscall.Errno {
	f, ok := fh.(*mountFile)
	if !ok {
		return 0
	}
	_, errno := f.flush()
	return errno
}

func (n *mountNode) Fsync(ctx context.Context, fh fs.FileHandle, _ uint32) syscall.Errno {
	f, ok := fh.(*mountFile)
	if !ok {
		return 0
	}
	return f.fsync(ctx)
}

func (n *mountNode) Mkdir(ctx context.Context, name string, mode uint32,
	out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	resp, errno := n.mfs.mutate(ctx, &common.MountRequest{Op: common.MountOpMkdir, Path: n.childPath(name), Mode: mode})
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, resp.Attr, out), 0
}

func (n *mountNode) Unlink(ctx context.Context, name string) syscall.Errno {
	_, errno := n.mfs.mutate(ctx, &common.MountRequest{Op: common.MountOpUnlink, Path: n.childPath(name)})
	return errno
}

func (n *mountNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	_, errno := n.mfs.mutate(ctx, &common.MountRequest{Op: common.MountOpRmdir, Path: n.childPath(name)})
	return errno
}

func (n *mountNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string,
	flags uint32) syscall.Errno {
	if flags&^unix.RENAME_NOREPLACE != 0 {
		return syscall.ENOTSUP // RENAME_EXCHANGE, RENAME_WHITEOUT
	}
	parent, ok := newParent.(*mountNode)
	if !ok {
		return syscall.EXDEV
	}
	_, errno := n.mfs.mutate(ctx, &common.MountRequest{
		Op:        common.MountOpRename,
		Path:      n.childPath(name),
		NewPath:   parent.childPath(newName),
		NoReplace: flags&unix.RENAME_NOREPLACE != 0,
	})
	return errno
}

func (n *mountNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	resp, errno := n.mfs.mutate(ctx, &common.MountRequest{
		Op: common.MountOpSymlink, Path: n.childPath(name), Target: target,
	})
	if errno != 0 {
		return nil, errno
	}
	return n.newChild(ctx, resp.Attr, out), 0
}

func (n *mountNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	resp, errno := n.mfs.do(ctx, &common.MountRequest{Op: common.MountOpReadlink, Path: n.remotePath()})
	if errno != 0 {
		return nil, errno
	}
	return []byte(resp.Target), 0
}

func (n *mountNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	resp, errno := n.mfs.do(ctx, &common.MountRequest{Op: common.MountOpStatfs, Path: n.mfs.root})
	if errno != 0 {
		return errno
	}
	st := resp.Statfs
	out.Blocks, out.Bfree, out.Bavail = st.Blocks, st.Bfree, st.Bavail
	out.Files, out.Ffree = st.Files, st.Ffree
	out.Bsize, out.Frsize, out.NameLen = st.Bsize, st.Bsize, st.NameLen
	return 0
}
