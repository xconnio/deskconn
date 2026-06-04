package deskconn

import (
	"encoding/json"
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
	return &indexDB{db: db}, nil
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

func (d *indexDB) entries(targetBuckets [][]byte) ([]IndexEntry, error) {
	var entries []IndexEntry
	err := d.db.View(func(tx *bolt.Tx) error {
		thumbs := tx.Bucket([]byte(bucketThumbnails))
		for _, bname := range targetBuckets {
			b := tx.Bucket(bname)
			if b == nil {
				continue
			}
			entries = append(entries, scanBucket(b, thumbs)...)
		}
		return nil
	})
	return entries, err
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

func scanBucket(b, thumbs *bolt.Bucket) []IndexEntry {
	var entries []IndexEntry
	_ = b.ForEach(func(k, v []byte) error {
		var entry IndexEntry
		if json.Unmarshal(v, &entry) == nil {
			if thumbs != nil {
				if thumb := thumbs.Get(k); len(thumb) > 0 {
					entry.Thumbnail = string(thumb)
				}
			}
			entries = append(entries, entry)
		}
		return nil
	})
	return entries
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
	return append(buckets(), []byte(bucketThumbnails), []byte(bucketMeta))
}

func (d *indexDB) close() {
	_ = d.db.Close()
}
