package deskconnd

import (
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
)

// thumbnailWorkers caps concurrent decodes/ffmpeg/pdftocairo runs across all callers,
// and is how many goroutines work through the prefetch queue: half the cores, at most 4,
// so thumbnailing a big folder never takes the whole machine.
//
//nolint:gochecknoglobals // fixed at startup
var thumbnailWorkers = min(4, max(1, runtime.NumCPU()/2))

const (
	// maxThumbnailQueue bounds the prefetch queue (a few pages), e.g. for a client scrolling
	// through a huge folder: its oldest pages' work is dropped rather than piled up.
	maxThumbnailQueue = 500
	// thumbnailSweepInterval is how often thumbnails of deleted or changed files are removed.
	thumbnailSweepInterval = 24 * time.Hour
	thumbnailExt           = ".jpg"
)

// thumbnailCache keeps generated thumbnails as JPEG files in dir, mirroring the source
// file's path (/home/u/a.png -> dir/home/u/a.png.jpg) and stamped with its modification
// time, so each is made once: a folder's thumbnails start generating when it's first
// opened and are just read back on later visits. A changed file's thumbnail is remade in
// place; sweep removes those of files that are gone or changed since.
type thumbnailCache struct {
	dir string
	sem chan struct{}

	mu       sync.Mutex
	inflight map[string]chan struct{}
	// queue is what's waiting for prefetch, the next one last.
	queue   []queuedThumbnail
	pending *sync.Cond
	workers sync.Once
}

// queuedThumbnail is a thumbnail to prefetch for the client session caller.
type queuedThumbnail struct {
	path   string
	caller uint64
}

func newThumbnailCache(cfgDirectory string) *thumbnailCache {
	c := &thumbnailCache{
		dir:      filepath.Join(cfgDirectory, "cache", "thumbnails"),
		sem:      make(chan struct{}, thumbnailWorkers),
		inflight: map[string]chan struct{}{},
	}
	c.pending = sync.NewCond(&c.mu)
	return c
}

// prefetch queues, ahead of everything queued before, making the thumbnails of entries
// (a page of a folder caller browsed) for the workers to do in the background. A new
// folder (its first page) drops what's still queued of caller's previous one: it moved on.
// What's already being made is let finish, at most thumbnailWorkers.
func (c *thumbnailCache) prefetch(caller uint64, entries []common.FileEntry, newFolder bool) {
	c.workers.Do(func() {
		for range thumbnailWorkers {
			common.SafeGo(c.work)
		}
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	if newFolder {
		c.queue = slices.DeleteFunc(c.queue, func(q queuedThumbnail) bool { return q.caller == caller })
	}
	// Reversed, since the queue is taken from its end: entries come out in listing order.
	for _, e := range slices.Backward(entries) {
		if e.IsDir || e.IsSymlink || thumbnailGenerator(e.Name, e.Size) == nil {
			continue
		}
		c.queue = slices.DeleteFunc(c.queue, func(q queuedThumbnail) bool { return q.path == e.Path })
		c.queue = append(c.queue, queuedThumbnail{path: e.Path, caller: caller})
	}
	if over := len(c.queue) - maxThumbnailQueue; over > 0 {
		c.queue = slices.Delete(c.queue, 0, over)
	}
	c.pending.Broadcast()
}

// work makes the queued thumbnails, newest first, for the process's lifetime.
func (c *thumbnailCache) work() {
	for {
		c.mu.Lock()
		for len(c.queue) == 0 {
			c.pending.Wait()
		}
		next := c.queue[len(c.queue)-1]
		c.queue = c.queue[:len(c.queue)-1]
		c.mu.Unlock()
		c.get(next.path)
	}
}

// getAll returns {path: base64 JPEG} for those of paths that have a thumbnail, making
// the missing ones first.
func (c *thumbnailCache) getAll(paths []string) map[string]string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	thumbs := map[string]string{}
	for _, path := range paths {
		wg.Add(1)
		common.SafeGo(func() {
			defer wg.Done()
			if thumb := c.get(path); thumb != "" {
				mu.Lock()
				thumbs[path] = thumb
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return thumbs
}

// cached returns path's thumbnail as base64 JPEG if one is cached, never making it.
func (c *thumbnailCache) cached(path string) string {
	path, info, generate := c.lookup(path)
	if generate == nil {
		return ""
	}
	thumb, _ := c.read(c.file(path), info)
	return thumb
}

// get returns path's thumbnail as base64 JPEG, or "" if it has none, making it if it isn't
// cached. A thumbnail being made for another caller is waited for rather than made twice.
func (c *thumbnailCache) get(path string) string {
	path, info, generate := c.lookup(path)
	if generate == nil {
		return ""
	}
	file := c.file(path)

	for {
		if thumb, ok := c.read(file, info); ok {
			return thumb
		}

		c.mu.Lock()
		if wait, ok := c.inflight[file]; ok {
			c.mu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		c.inflight[file] = done
		c.mu.Unlock()

		thumb := c.make(generate, path, file, info.ModTime())

		c.mu.Lock()
		delete(c.inflight, file)
		c.mu.Unlock()
		close(done)
		return thumb
	}
}

// lookup cleans path and, if it gets a thumbnail, returns its info and what makes it;
// generate is nil otherwise. The cache's own files get none: thumbnails of thumbnails
// would nest another level on every visit.
func (c *thumbnailCache) lookup(path string) (string, os.FileInfo, func(string) string) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || strings.HasPrefix(path, c.dir+string(filepath.Separator)) {
		return path, nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return path, nil, nil
	}
	return path, info, thumbnailGenerator(info.Name(), info.Size())
}

// read returns the thumbnail cached in file if it's of the source's current version (info).
// An empty file is a cached "none": tried before, and the source has no thumbnail
// (undecodable, no ffmpeg...).
func (c *thumbnailCache) read(file string, info os.FileInfo) (string, bool) {
	cached, err := os.Stat(file)
	if err != nil || !sameModTime(cached, info) {
		return "", false
	}
	//nolint:gosec // file is path mirrored inside c.dir
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(data), true
}

// make generates path's thumbnail and saves it to file, stamped with modTime (the source's).
func (c *thumbnailCache) make(generate func(string) string, path, file string, modTime time.Time) string {
	c.sem <- struct{}{}
	thumb := generate(path)
	<-c.sem

	data, err := base64.StdEncoding.DecodeString(thumb)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return thumb
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".tmp-*")
	if err != nil {
		return thumb
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Chtimes(tmp.Name(), modTime, modTime) != nil ||
		os.Rename(tmp.Name(), file) != nil {
		_ = os.Remove(tmp.Name())
	}
	return thumb
}

// sweepLoop sweeps now and then every thumbnailSweepInterval, for the process's lifetime.
func (c *thumbnailCache) sweepLoop() {
	for {
		c.sweep()
		time.Sleep(thumbnailSweepInterval)
	}
}

// sweep removes the thumbnails of files that no longer exist or changed since (they're
// remade on their folder's next visit), leftover temp files, and directories left empty.
func (c *thumbnailCache) sweep() {
	// Removes go through root, so nothing in the cache can lead them outside it.
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return
	}
	defer root.Close()

	removed := 0
	_ = filepath.WalkDir(c.dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable dirs, keep sweeping
		}
		c.mu.Lock()
		_, busy := c.inflight[file]
		c.mu.Unlock()
		if busy {
			return nil
		}
		rel, err := filepath.Rel(c.dir, file)
		if err == nil && c.isStale(file, d) && root.Remove(rel) == nil {
			removed++
			// Up from rel's dir, stopping at the first that isn't empty.
			dir := filepath.Dir(rel)
			for dir != "." && root.Remove(dir) == nil {
				dir = filepath.Dir(dir)
			}
		}
		return nil
	})
	if removed > 0 {
		log.Debugf("thumbnails: removed %d stale", removed)
	}
}

func (c *thumbnailCache) isStale(file string, d fs.DirEntry) bool {
	source, ok := c.source(file)
	if !ok {
		// A leftover temp file, or not one of ours.
		info, err := d.Info()
		return err != nil || time.Since(info.ModTime()) > time.Hour
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil || !sourceInfo.Mode().IsRegular() {
		return true
	}
	cached, err := d.Info()
	return err != nil || !sameModTime(cached, sourceInfo)
}

// file is where the thumbnail of path (absolute, clean) is kept, see cacheRel.
func (c *thumbnailCache) file(path string) string {
	return filepath.Join(c.dir, cacheRel(path, runtime.GOOS == "windows")+thumbnailExt)
}

// source is file's inverse: the path whose thumbnail file is, if it is one.
func (c *thumbnailCache) source(file string) (string, bool) {
	rel, err := filepath.Rel(c.dir, file)
	if err != nil || !strings.HasSuffix(rel, thumbnailExt) || strings.HasPrefix(filepath.Base(rel), ".tmp-") {
		return "", false
	}
	return sourcePath(strings.TrimSuffix(rel, thumbnailExt), runtime.GOOS == "windows")
}

// cacheRel is where, relative to the cache dir, an absolute path's thumbnail is kept:
// /home/u/a.png -> home/u/a.png; on Windows a drive loses its colon (C:\a.png -> C\a.png)
// and a network share gets a UNC dir (\\srv\share\a.png -> UNC\srv\share\a.png), so
// sourcePath can tell the two apart.
func cacheRel(path string, windows bool) string {
	switch {
	case !windows:
		return strings.TrimPrefix(path, "/")
	case strings.HasPrefix(path, `\\`):
		return `UNC\` + path[2:]
	case len(path) >= 2 && path[1] == ':':
		return path[:1] + path[2:]
	}
	return path
}

// sourcePath is cacheRel's inverse.
func sourcePath(rel string, windows bool) (string, bool) {
	switch {
	case !windows:
		return "/" + rel, true
	case strings.HasPrefix(rel, `UNC\`):
		return `\\` + rel[4:], true
	case len(rel) >= 2 && rel[1] == '\\':
		return rel[:1] + ":" + rel[1:], true
	}
	return "", false
}

// sameModTime compares to the second: some filesystems don't keep finer times on Chtimes.
func sameModTime(a, b os.FileInfo) bool {
	return a.ModTime().Unix() == b.ModTime().Unix()
}

// thumbnailGenerator returns what makes the thumbnail of a file named name, or nil if it
// gets none.
func thumbnailGenerator(name string, size int64) func(string) string {
	switch {
	case isImageFile(name) && size <= common.MaxThumbnailSourceSize:
		return common.ImageThumbnail
	case isVideoFile(name):
		return common.VideoThumbnail
	case isPDFFile(name):
		return common.PDFThumbnail
	}
	return nil
}
