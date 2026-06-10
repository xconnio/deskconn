package deskconn

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

const (
	bucketThumbnails = "thumbnails"
	bucketMeta       = "meta"
)

type indexDB struct {
	db *bolt.DB
}

func newIndexDB(cfgDirectory string) (*indexDB, error) {
	dbPath := filepath.Join(cfgDirectory, "fileindex.db")
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(initBuckets); err != nil {
		_ = db.Close()
		return nil, err
	}
	idb := &indexDB{db: db}
	if err := idb.ensureOrderIndexes(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idb, nil
}

func (d *indexDB) isIndexingComplete() bool {
	var ready bool
	_ = d.db.View(func(tx *bolt.Tx) error {
		ready = string(tx.Bucket([]byte(bucketMeta)).Get([]byte("status"))) == indexStatusReady
		return nil
	})
	return ready
}

func (d *indexDB) markIndexingComplete() error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketMeta)).Put([]byte("status"), []byte(indexStatusReady))
	})
}

func (d *indexDB) addEntry(entry IndexEntry) error {
	bucket := bucketForCategory(entry.Category)
	if bucket == nil {
		return nil
	}
	entry.Thumbnail = ""
	return d.db.Update(func(tx *bolt.Tx) error {
		order := tx.Bucket(orderBucketName(bucket))
		if old, found := readEntry(tx, bucket, entry.Path); found {
			if err := order.Delete(orderKey(old)); err != nil {
				return err
			}
		}
		if err := order.Put(orderKey(entry), []byte(entry.Path)); err != nil {
			return err
		}
		return updateEntry(tx, bucket, entry)
	})
}

func (d *indexDB) addThumbnail(path, thumb string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketThumbnails)).Put([]byte(path), []byte(thumb))
	})
}

func (d *indexDB) removeEntry(path string) {
	_ = d.db.Update(func(tx *bolt.Tx) error {
		deleteEntry(tx, path)
		return nil
	})
}

func (d *indexDB) isEntryUpToDate(path string, modTime time.Time, category string) bool {
	bucket := bucketForCategory(category)
	if bucket == nil {
		return false
	}
	var ok bool
	_ = d.db.View(func(tx *bolt.Tx) error {
		e, found := readEntry(tx, bucket, path)
		ok = found && e.ModTime.Unix() == modTime.Unix()
		return nil
	})
	return ok
}

func (d *indexDB) removeStaleEntries() {
	var stale [][]byte
	_ = d.db.View(func(tx *bolt.Tx) error {
		stale = collectStaleKeys(tx)
		return nil
	})
	if len(stale) == 0 {
		return
	}
	_ = d.db.Update(func(tx *bolt.Tx) error {
		deleteKeys(tx, stale)
		return nil
	})
	log.Printf("fileindex: pruned %d stale entries", len(stale))
}

// orderCursor tracks the current position of one category's order-index cursor
// during a k-way merge.
type orderCursor struct {
	cursor *bolt.Cursor
	key    []byte
	val    []byte
}

// queryEntries returns up to limit entries across targetBuckets, sorted by
// modification time (newest first), merging the per-category order indexes.
// cursor (if non-empty) resumes iteration from the position returned as
// nextCursor by a previous call. hasMore reports whether further entries
// remain.
func (d *indexDB) queryEntries(targetBuckets [][]byte, cursor map[string]string, limit int) (
	entries []IndexEntry, nextCursor map[string]string, hasMore bool, err error,
) {
	nextCursor = make(map[string]string)

	err = d.db.View(func(tx *bolt.Tx) error {
		thumbs := tx.Bucket([]byte(bucketThumbnails))
		cursors := make(map[string]*orderCursor, len(targetBuckets))

		for _, bname := range targetBuckets {
			ob := tx.Bucket(orderBucketName(bname))
			if ob == nil {
				continue
			}
			c := ob.Cursor()
			var k, v []byte
			if last, ok := cursor[string(bname)]; ok && last != "" {
				lastKey, derr := base64.StdEncoding.DecodeString(last)
				if derr != nil {
					return derr
				}
				// lastKey points at the next unconsumed item; if it was
				// removed since the previous page, Seek lands on the item
				// that took its place.
				k, v = c.Seek(lastKey)
			} else {
				k, v = c.First()
			}
			if k != nil {
				cursors[string(bname)] = &orderCursor{
					cursor: c,
					key:    append([]byte(nil), k...),
					val:    append([]byte(nil), v...),
				}
			}
		}

		for len(entries) < limit {
			var bestCat string
			var best *orderCursor
			for cat, oc := range cursors {
				if best == nil || bytes.Compare(oc.key, best.key) < 0 {
					best = oc
					bestCat = cat
				}
			}
			if best == nil {
				break
			}

			if e, found := readEntry(tx, []byte(bestCat), string(best.val)); found {
				if thumbs != nil {
					if thumb := thumbs.Get(best.val); len(thumb) > 0 {
						e.Thumbnail = string(thumb)
					}
				}
				entries = append(entries, e)
			}

			if k, v := best.cursor.Next(); k != nil {
				best.key = append(best.key[:0], k...)
				best.val = append(best.val[:0], v...)
			} else {
				delete(cursors, bestCat)
			}
		}

		for cat, oc := range cursors {
			nextCursor[cat] = base64.StdEncoding.EncodeToString(oc.key)
			hasMore = true
		}
		return nil
	})

	return entries, nextCursor, hasMore, err
}

func initBuckets(tx *bolt.Tx) error {
	for _, name := range allBuckets() {
		if _, err := tx.CreateBucketIfNotExists(name); err != nil {
			return err
		}
	}
	return nil
}

func updateEntry(tx *bolt.Tx, bucket []byte, entry IndexEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(entry.Path), data)
}

func readEntry(tx *bolt.Tx, bucket []byte, path string) (IndexEntry, bool) {
	v := tx.Bucket(bucket).Get([]byte(path))
	if v == nil {
		return IndexEntry{}, false
	}
	var e IndexEntry
	if json.Unmarshal(v, &e) != nil {
		return IndexEntry{}, false
	}
	return e, true
}

func deleteEntry(tx *bolt.Tx, path string) {
	key := []byte(path)
	for _, bname := range buckets() {
		if old, found := readEntry(tx, bname, path); found {
			_ = tx.Bucket(orderBucketName(bname)).Delete(orderKey(old))
		}
		_ = tx.Bucket(bname).Delete(key)
	}
	_ = tx.Bucket([]byte(bucketThumbnails)).Delete(key)
}

func collectStaleKeys(tx *bolt.Tx) [][]byte {
	var stale [][]byte
	for _, bname := range buckets() {
		_ = tx.Bucket(bname).ForEach(func(k, _ []byte) error {
			if _, err := os.Lstat(string(k)); os.IsNotExist(err) {
				stale = append(stale, append([]byte(nil), k...))
			}
			return nil
		})
	}
	return stale
}

func deleteKeys(tx *bolt.Tx, keys [][]byte) {
	for _, key := range keys {
		deleteEntry(tx, string(key))
	}
}

// mtimeKey returns an 8-byte big-endian key that sorts in descending order
// of modification time (newest first) when compared lexicographically.
func mtimeKey(modTime time.Time) []byte {
	inverted := int64(math.MaxInt64) - modTime.UnixNano()
	if inverted < 0 {
		inverted = 0
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(inverted)) //nolint:gosec // inverted is non-negative by construction
	return buf
}

// orderKey returns the key used in a category's order index for entry,
// composed of its mtime key followed by its path (for stable tie-breaking).
func orderKey(entry IndexEntry) []byte {
	return append(mtimeKey(entry.ModTime), []byte(entry.Path)...)
}

// orderBucketName returns the name of the order-index bucket paired with
// the given category bucket.
func orderBucketName(category []byte) []byte {
	return []byte(string(category) + "_order")
}

// ensureOrderIndexes builds the order-index bucket for any category whose
// index is empty but whose entries are not, populating it from the existing
// entries. This migrates databases created before order indexes existed.
func (d *indexDB) ensureOrderIndexes() error {
	return d.db.Update(func(tx *bolt.Tx) error {
		for _, bname := range buckets() {
			ob := tx.Bucket(orderBucketName(bname))
			if ob.Stats().KeyN > 0 {
				continue
			}
			b := tx.Bucket(bname)
			err := b.ForEach(func(_, v []byte) error {
				var e IndexEntry
				if json.Unmarshal(v, &e) != nil {
					return nil //nolint:nilerr // skip entries we can't decode, continue migrating
				}
				return ob.Put(orderKey(e), []byte(e.Path))
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func bucketForCategory(cat string) []byte {
	switch cat {
	case CategoryImages:
		return []byte(CategoryImages)
	case CategoryVideos:
		return []byte(CategoryVideos)
	case CategoryPDFs:
		return []byte(CategoryPDFs)
	case CategoryTexts:
		return []byte(CategoryTexts)
	case CategoryDocuments:
		return []byte(CategoryDocuments)
	}
	return nil
}

func buckets() [][]byte {
	return [][]byte{
		[]byte(CategoryImages),
		[]byte(CategoryVideos),
		[]byte(CategoryPDFs),
		[]byte(CategoryTexts),
		[]byte(CategoryDocuments),
	}
}

func allBuckets() [][]byte {
	bs := buckets()
	all := make([][]byte, 0, len(bs)*2+2)
	all = append(all, bs...)
	all = append(all, []byte(bucketThumbnails), []byte(bucketMeta))
	for _, b := range bs {
		all = append(all, orderBucketName(b))
	}
	return all
}

func (d *indexDB) close() {
	_ = d.db.Close()
}
