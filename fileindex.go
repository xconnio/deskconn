package deskconn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

const (
	CategoryImages    = "images"
	CategoryVideos    = "videos"
	CategoryPDFs      = "pdfs"
	CategoryTexts     = "texts"
	CategoryDocuments = "documents"

	indexStatusIndexing = "indexing"
	indexStatusReady    = "ready"

	// How many files to process before sleeping to keep CPU usage low.
	indexBatchSize  = 10
	indexBatchSleep = 100 * time.Millisecond

	// Pagination defaults for Query.
	defaultIndexLimit = 100
	maxIndexLimit     = 500
	// Extra pause after each thumbnail generation (ffmpeg/decode can be heavy).
	thumbIndexSleep = 150 * time.Millisecond
)

type IndexEntry struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	Thumbnail string    `json:"thumbnail,omitempty"`
}

type IndexQueryResult struct {
	Status     string            `json:"status"`
	Entries    []IndexEntry      `json:"entries,omitempty"`
	NextCursor map[string]string `json:"next_cursor,omitempty"`
	HasMore    bool              `json:"has_more"`
}

type IndexService struct {
	db      *indexDB
	watcher *fsnotify.Watcher
	homeDir string

	ready atomic.Bool
	once  sync.Once
}

// NewIndexService opens (or creates) the bbolt database in cfgDirectory and
// restores the previous indexing state if one exists.
func NewIndexService(cfgDirectory string) (*IndexService, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	indexDb, err := newIndexDB(cfgDirectory)
	if err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		indexDb.close()
		return nil, err
	}

	svc := &IndexService{
		db:      indexDb,
		watcher: watcher,
		homeDir: homeDir,
	}
	svc.ready.Store(indexDb.isIndexingComplete())

	return svc, nil
}

// Start launches the background watcher and either the initial indexer (first
// run) or an incremental sync (subsequent restarts). Guarded by sync.Once.
func (s *IndexService) Start(ctx context.Context) {
	s.once.Do(func() {
		go s.runWatcher(ctx)
		if !s.ready.Load() {
			go s.runIndexer(ctx)
		} else {
			go s.runSync(ctx)
		}
	})
}

// Close releases the watcher and the bbolt database.
func (s *IndexService) Close() {
	_ = s.watcher.Close()
	s.db.close()
}

// Query returns a page of indexed entries across the given categories,
// sorted by modification time (newest first). If categories is empty, all
// categories are queried. If indexing is still in progress it returns
// {status:"indexing"} with no entries.
//
// cursor resumes iteration from a previous call's NextCursor. limit caps the
// number of entries returned; values <= 0 or > maxIndexLimit fall back to
// defaultIndexLimit.
func (s *IndexService) Query(categories []string, cursor map[string]string, limit int) (*IndexQueryResult, error) {
	if !s.ready.Load() {
		return &IndexQueryResult{Status: indexStatusIndexing}, nil
	}

	if limit <= 0 || limit > maxIndexLimit {
		limit = defaultIndexLimit
	}

	var targetBuckets [][]byte
	for _, category := range categories {
		if b := bucketForCategory(category); b != nil {
			targetBuckets = append(targetBuckets, b)
		}
	}
	if len(targetBuckets) == 0 {
		targetBuckets = buckets()
	}

	entries, nextCursor, hasMore, err := s.db.queryEntries(targetBuckets, cursor, limit)
	if err != nil {
		return nil, err
	}

	result := &IndexQueryResult{
		Status:  indexStatusReady,
		Entries: entries,
		HasMore: hasMore,
	}
	if hasMore {
		result.NextCursor = nextCursor
	}
	return result, nil
}

// indexRoots returns the subset of the five well-known user folders that
// actually exist on disk. Only these directories are indexed and watched.
func (s *IndexService) indexRoots() []string {
	candidates := []string{"Desktop", "Downloads", "Documents", "Pictures", "Videos"}
	roots := make([]string, 0, len(candidates))
	for _, name := range candidates {
		p := filepath.Join(s.homeDir, name)
		if _, err := os.Stat(p); err == nil {
			roots = append(roots, p)
		}
	}
	return roots
}

func (s *IndexService) runIndexer(ctx context.Context) {
	roots := s.indexRoots()
	if len(roots) == 0 {
		log.Println("fileindex: no standard folders found, skipping indexing")
		s.ready.Store(true)
		return
	}
	log.Printf("fileindex: starting background indexing of %v", roots)

	batchCount := 0
	s.walkFileEntries(ctx, roots, func(path string, entry IndexEntry) {
		if err := s.db.addEntry(entry); err != nil {
			log.Printf("fileindex: store %s: %v", path, err)
		}
		s.storeThumbnailSlow(path, entry.Category, entry.Size)
		batchCount++
		if batchCount >= indexBatchSize {
			batchCount = 0
			time.Sleep(indexBatchSleep)
		}
	})

	if ctx.Err() == nil {
		_ = s.db.markIndexingComplete()
		s.ready.Store(true)
		log.Println("fileindex: indexing complete")
	}
}

// runSync is called on restart when the index is already marked ready. It
// walks the indexed roots and reconciles the DB against the current filesystem:
//   - files added or modified while the daemon was offline are indexed
//   - DB entries for files that no longer exist are pruned
func (s *IndexService) runSync(ctx context.Context) {
	log.Println("fileindex: syncing changes since last run")

	batchCount := 0
	s.walkFileEntries(ctx, s.indexRoots(), func(path string, entry IndexEntry) {
		if s.db.isEntryUpToDate(path, entry.ModTime, entry.Category) {
			return
		}
		if err := s.db.addEntry(entry); err != nil {
			log.Printf("fileindex: sync store %s: %v", path, err)
		}
		s.storeThumbnailSlow(path, entry.Category, entry.Size)
		batchCount++
		if batchCount >= indexBatchSize {
			batchCount = 0
			time.Sleep(indexBatchSleep)
		}
	})

	if ctx.Err() == nil {
		s.db.removeStaleEntries()
		log.Println("fileindex: sync complete")
	}
}

func (s *IndexService) runWatcher(ctx context.Context) {
	s.walkDirs(ctx, s.indexRoots(), func(path string) {
		_ = s.watcher.Add(path)
	})

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			s.handleWatchEvent(event)
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("fileindex: watcher error: %v", err)
		}
	}
}

func (s *IndexService) handleWatchEvent(event fsnotify.Event) {
	name := filepath.Base(event.Name)
	if strings.HasPrefix(name, ".") {
		return
	}

	// Rename fires on the old path — treat it as a remove; the new path gets a Create.
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		s.db.removeEntry(event.Name)
		return
	}

	if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
		return
	}

	info, err := os.Lstat(event.Name)
	if err != nil {
		return
	}

	if info.IsDir() {
		_ = s.watcher.Add(event.Name)
		return
	}

	category := fileCategory(name)
	if category == "" {
		return
	}

	entry := IndexEntry{
		Path:     event.Name,
		Name:     name,
		Category: category,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}

	if err := s.db.addEntry(entry); err != nil {
		log.Printf("fileindex: watch store %s: %v", event.Name, err)
		return
	}

	if category == CategoryImages && info.Size() <= maxThumbnailSourceSize {
		if thumb := generateThumbnail(event.Name); thumb != "" {
			_ = s.db.addThumbnail(event.Name, thumb)
		}
	} else if category == CategoryVideos {
		if thumb := generateVideoThumbnail(event.Name); thumb != "" {
			_ = s.db.addThumbnail(event.Name, thumb)
		}
	}
}

// walkFileEntries walks roots and calls fn for each non-hidden file with a known category.
// It handles context cancellation, hidden entry filtering, and walk error logging.
func (s *IndexService) walkFileEntries(ctx context.Context, roots []string, fn func(path string, entry IndexEntry)) {
	for _, root := range roots {
		if ctx.Err() != nil {
			return
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil //nolint:nilerr // skip unreadable paths, continue walking
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			category := fileCategory(d.Name())
			if category == "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr // skip files we can't stat, continue walking
			}
			fn(path, IndexEntry{
				Path:     path,
				Name:     d.Name(),
				Category: category,
				Size:     info.Size(),
				ModTime:  info.ModTime(),
			})
			return nil
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("fileindex: walk %s: %v", root, err)
		}
	}
}

// walkDirs walks roots and calls fn for each non-hidden directory (used to register watcher paths).
func (s *IndexService) walkDirs(ctx context.Context, roots []string, fn func(path string)) {
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil //nolint:nilerr // skip unreadable paths, continue walking
			}
			if !d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			fn(path)
			return nil
		})
	}
}

// storeThumbnailSlow generates and stores a thumbnail then sleeps to limit CPU usage during bulk walks.
func (s *IndexService) storeThumbnailSlow(path, category string, size int64) {
	switch {
	case category == CategoryImages && size <= maxThumbnailSourceSize:
		if thumb := generateThumbnail(path); thumb != "" {
			_ = s.db.addThumbnail(path, thumb)
		}
		time.Sleep(thumbIndexSleep)
	case category == CategoryVideos:
		if thumb := generateVideoThumbnail(path); thumb != "" {
			_ = s.db.addThumbnail(path, thumb)
		}
		time.Sleep(thumbIndexSleep)
	}
}

func fileCategory(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".ico", ".tiff", ".tif", ".heic", ".heif":
		return CategoryImages
	case ".mp4", ".webm", ".mov", ".avi", ".mkv", ".ogv", ".flv", ".wmv", ".m4v", ".3gp":
		return CategoryVideos
	case ".pdf":
		return CategoryPDFs
	case ".txt", ".md", ".log", ".csv", ".json", ".yaml", ".yml", ".toml",
		".ini", ".cfg", ".xml", ".html", ".htm", ".rst":
		return CategoryTexts
	case ".doc", ".docx", ".odt", ".rtf", ".xls", ".xlsx", ".ods",
		".ppt", ".pptx", ".odp", ".pages", ".numbers", ".key", ".epub":
		return CategoryDocuments
	}
	return ""
}
