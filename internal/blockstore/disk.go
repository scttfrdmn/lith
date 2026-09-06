// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zeebo/xxh3"
)

// diskTier is a byte-bounded on-disk block cache: one file per block under
// <root>/<2-hex>/<2-hex>/<hash>. The block's etagHash is part of the cache key
// (and thus the filename), so a rebuilt index with a changed ETag never reads
// a stale block. Eviction sweeps files by mtime (LRU), oldest first, whenever
// the tier exceeds its capacity.
type diskTier struct {
	root     string
	capacity int64

	mu   sync.Mutex
	size int64
}

func newDiskTier(root string, capacity int64) (*diskTier, error) {
	if capacity <= 0 {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("blockstore: create disk cache %s: %w", root, err)
	}
	d := &diskTier{root: root, capacity: capacity}
	d.size = d.scanSize()
	return d, nil
}

// path returns the on-disk path for a cache key, sharded two levels deep.
func (d *diskTier) path(cacheKey string) string {
	h := fmt.Sprintf("%016x", xxh3.HashString(cacheKey))
	return filepath.Join(d.root, h[0:2], h[2:4], h)
}

// Get returns the cached block and whether it was present, touching the file's
// mtime on a hit so the LRU sweep treats it as recently used.
func (d *diskTier) Get(cacheKey string) ([]byte, bool) {
	if d == nil {
		return nil, false
	}
	p := d.path(cacheKey)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return data, true
}

// Put writes a block to disk atomically and evicts if over capacity.
func (d *diskTier) Put(cacheKey string, data []byte) {
	if d == nil || int64(len(data)) > d.capacity {
		return
	}
	p := d.path(cacheKey)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".blk-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return
	}

	d.mu.Lock()
	d.size += int64(len(data))
	over := d.size > d.capacity
	d.mu.Unlock()
	if over {
		d.evict()
	}
}

type diskEntry struct {
	path  string
	size  int64
	mtime time.Time
}

// evict deletes the least-recently-used files (by mtime) until the tier is
// under 90% of capacity.
func (d *diskTier) evict() {
	d.mu.Lock()
	defer d.mu.Unlock()

	var entries []diskEntry
	var total int64
	_ = filepath.Walk(d.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		entries = append(entries, diskEntry{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
		return nil
	})
	d.size = total

	target := d.capacity * 9 / 10
	if total <= target {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	for _, e := range entries {
		if total <= target {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
	d.size = total
}

// scanSize sums the bytes currently on disk (called once at open).
func (d *diskTier) scanSize() int64 {
	var total int64
	_ = filepath.Walk(d.root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
