// Package thumbcache is a small size-capped on-disk cache for camera images.
package thumbcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultMax is the size cap used when Cache.Max is zero.
const DefaultMax = 256 << 20

// Cache stores blobs by key in Dir. A nil Cache or an empty Dir caches nothing.
type Cache struct {
	Dir string
	Max int64 // total bytes; 0 means DefaultMax

	mu sync.Mutex // serializes eviction scans
}

// Key derives a file name from the content identity. Content IDs are not
// stable across connections, so path, size and creation time stand in for them.
func Key(slot uint32, path string, size uint64, created time.Time, kind string) string {
	s := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%d|%s", slot, path, size, created.UnixMilli(), kind)))
	return hex.EncodeToString(s[:])
}

// Get returns the cached blob and marks it as recently used.
func (c *Cache) Get(key string) ([]byte, bool) {
	if c == nil || c.Dir == "" {
		return nil, false
	}
	p := filepath.Join(c.Dir, key)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	os.Chtimes(p, now, now) // eviction goes by mtime
	return b, true
}

// Put writes the blob atomically and evicts the oldest files while over the cap.
func (c *Cache) Put(key string, data []byte) error {
	if c == nil || c.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.Dir, ".tmp-")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(c.Dir, key))
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	c.evict()
	return nil
}

func (c *Cache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	max := c.Max
	if max <= 0 {
		max = DefaultMax
	}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		size int64
		mod  time.Time
	}
	var fs []file
	var total int64
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || e.Name()[0] == '.' { // dot files are in-flight temps
			continue
		}
		fs = append(fs, file{e.Name(), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.Before(fs[j].mod) })
	for _, f := range fs {
		if total <= max {
			return
		}
		if os.Remove(filepath.Join(c.Dir, f.name)) == nil {
			total -= f.size
		}
	}
}
