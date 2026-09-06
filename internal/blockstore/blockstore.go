// SPDX-License-Identifier: Apache-2.0

// Package blockstore implements lith's read-only block cache over S3 range
// GETs: a byte-bounded 2Q memory tier, an NVMe disk tier, contiguous-miss
// coalescing, singleflight de-duplication, a bounded worker pool that favors
// demand reads over prefetch, and ETag-mismatch detection. See the pinned
// Design issue, §4.2.
package blockstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zeebo/xxh3"
)

// ErrStale is returned when a fetched block's ETag does not match the index;
// the FUSE layer maps it to EIO. The key is recorded in the stale set.
var ErrStale = errors.New("blockstore: object changed since index build (ETag mismatch)")

// Source fetches byte ranges from the backing store (S3 or a fake). It matches
// s3client.API's GetRange.
type Source interface {
	GetRange(ctx context.Context, key string, off, length int64) (data []byte, etag string, err error)
}

// Recorder receives cache and S3 events for metrics. A nil Recorder is fine;
// all calls are guarded.
type Recorder interface {
	MemHit()
	DiskHit()
	Miss()
	S3Get(bytes int64, err bool)
	StaleKey(key string)
	// PrefetchIssued is called when a block is prefetched; PrefetchHit when a
	// demand read consumes a previously prefetched block (accuracy = hit/issued).
	PrefetchIssued()
	PrefetchHit()
}

// Key identifies an object plus the ETag hash recorded in the index.
type Key struct {
	Key      string
	ETagHash uint64
}

// Config configures a BlockStore.
type Config struct {
	Bucket        string
	BlockSize     int64
	MemCache      int64
	DiskCache     int64
	DiskPath      string
	MaxRange      int64
	S3Concurrency int
	Recorder      Recorder
}

// BlockStore is a read-only, tiered block cache.
type BlockStore struct {
	src       Source
	bucket    string
	blockSize int64
	maxRange  int64

	mem  *mem2Q
	disk *diskTier

	flight *flightGroup

	sem      chan struct{} // total S3 concurrency
	prefetch chan struct{} // sub-limit so prefetch cannot starve demand

	rec Recorder

	mu         sync.Mutex
	stale      map[string]struct{}
	prefetched map[string]struct{} // cache keys fetched by prefetch, not yet demand-read
}

// New builds a BlockStore over src.
func New(src Source, cfg Config) (*BlockStore, error) {
	if cfg.BlockSize <= 0 {
		return nil, fmt.Errorf("blockstore: block size must be > 0")
	}
	conc := cfg.S3Concurrency
	if conc <= 0 {
		conc = 64
	}
	maxRange := cfg.MaxRange
	if maxRange < cfg.BlockSize {
		maxRange = cfg.BlockSize
	}
	disk, err := newDiskTier(cfg.DiskPath, cfg.DiskCache)
	if err != nil {
		return nil, err
	}
	bs := &BlockStore{
		src:        src,
		bucket:     cfg.Bucket,
		blockSize:  cfg.BlockSize,
		maxRange:   maxRange,
		mem:        newMem2Q(cfg.MemCache),
		disk:       disk,
		flight:     newFlightGroup(),
		sem:        make(chan struct{}, conc),
		prefetch:   make(chan struct{}, max(1, conc/2)),
		rec:        cfg.Recorder,
		stale:      make(map[string]struct{}),
		prefetched: make(map[string]struct{}),
	}
	return bs, nil
}

// BlockSize returns the configured block size.
func (bs *BlockStore) BlockSize() int64 { return bs.blockSize }

func (bs *BlockStore) cacheKey(k Key, blockIdx int64) string {
	return fmt.Sprintf("%s|%s|%016x|%d", bucketTag(bs.bucket), k.Key, k.ETagHash, blockIdx)
}

func bucketTag(b string) string { return b }

// lookup returns a block from cache without recording metrics. tier is "mem",
// "disk", or "" (miss); a disk hit is promoted to memory.
func (bs *BlockStore) lookup(k Key, blockIdx int64) ([]byte, string) {
	ck := bs.cacheKey(k, blockIdx)
	if data, ok := bs.mem.Get(ck); ok {
		return data, "mem"
	}
	if data, ok := bs.disk.Get(ck); ok {
		bs.mem.Put(ck, data) // promote to memory
		return data, "disk"
	}
	return nil, ""
}

// demandCached does a cache lookup for a demand read, recording the hit tier
// and crediting prefetch accuracy when the block was prefetched.
func (bs *BlockStore) demandCached(k Key, blockIdx int64) ([]byte, bool) {
	data, tier := bs.lookup(k, blockIdx)
	switch tier {
	case "mem":
		bs.record(func(r Recorder) { r.MemHit() })
	case "disk":
		bs.record(func(r Recorder) { r.DiskHit() })
	default:
		return nil, false
	}
	bs.notePrefetchHit(bs.cacheKey(k, blockIdx))
	return data, true
}

// notePrefetchHit credits a prefetch as used the first time a demand read
// consumes a block that prefetch had fetched.
func (bs *BlockStore) notePrefetchHit(ck string) {
	bs.mu.Lock()
	_, ok := bs.prefetched[ck]
	if ok {
		delete(bs.prefetched, ck)
	}
	bs.mu.Unlock()
	if ok {
		bs.record(func(r Recorder) { r.PrefetchHit() })
	}
}

func (bs *BlockStore) storeBlock(k Key, blockIdx int64, data []byte) {
	ck := bs.cacheKey(k, blockIdx)
	bs.mem.Put(ck, data)
	bs.disk.Put(ck, data)
}

// Get returns a single block, serving from cache or fetching it (demand
// priority). objSize bounds the final block's length.
func (bs *BlockStore) Get(ctx context.Context, k Key, blockIdx, objSize int64) ([]byte, error) {
	if data, ok := bs.demandCached(k, blockIdx); ok {
		return data, nil
	}
	bs.record(func(r Recorder) { r.Miss() })
	blocks, err := bs.fetchRun(ctx, k, blockIdx, blockIdx, objSize, false)
	if err != nil {
		return nil, err
	}
	return blocks[0], nil
}

// Prefetch fetches a single block at lower priority than demand reads. Errors
// are swallowed (prefetch is best-effort).
func (bs *BlockStore) Prefetch(ctx context.Context, k Key, blockIdx, objSize int64) {
	if _, tier := bs.lookup(k, blockIdx); tier != "" {
		return // already cached; nothing to prefetch
	}
	if _, err := bs.fetchRun(ctx, k, blockIdx, blockIdx, objSize, true); err != nil {
		return
	}
	ck := bs.cacheKey(k, blockIdx)
	bs.mu.Lock()
	bs.prefetched[ck] = struct{}{}
	bs.mu.Unlock()
	bs.record(func(r Recorder) { r.PrefetchIssued() })
}

// GetRange returns bytes for [off, off+length) of the object. It gathers the
// covered blocks from cache and fetches contiguous misses in a single coalesced
// S3 GET (bounded by maxRange).
func (bs *BlockStore) GetRange(ctx context.Context, k Key, off, length, objSize int64) ([]byte, error) {
	if off >= objSize || length <= 0 {
		return []byte{}, nil
	}
	end := off + length
	if end > objSize {
		end = objSize
	}
	first := off / bs.blockSize
	last := (end - 1) / bs.blockSize
	blocks := make([][]byte, last-first+1)

	maxBlocks := bs.maxRange / bs.blockSize
	if maxBlocks < 1 {
		maxBlocks = 1
	}

	i := first
	for i <= last {
		if data, ok := bs.demandCached(k, i); ok {
			blocks[i-first] = data
			i++
			continue
		}
		bs.record(func(r Recorder) { r.Miss() })
		// Extend a run of contiguous, uncached blocks (bounded by maxBlocks).
		j := i
		for j+1 <= last && (j-i+1) < maxBlocks {
			if _, tier := bs.lookup(k, j+1); tier != "" {
				break
			}
			j++
		}
		fetched, err := bs.fetchRun(ctx, k, i, j, objSize, false)
		if err != nil {
			return nil, err
		}
		for b := i; b <= j; b++ {
			blocks[b-first] = fetched[b-i]
		}
		i = j + 1
	}

	// Assemble the requested byte range from the gathered blocks.
	out := make([]byte, 0, end-off)
	for b := first; b <= last; b++ {
		blkStart := b * bs.blockSize
		lo := int64(0)
		if off > blkStart {
			lo = off - blkStart
		}
		hi := int64(len(blocks[b-first]))
		if blkStart+hi > end {
			hi = end - blkStart
		}
		if lo < hi {
			out = append(out, blocks[b-first][lo:hi]...)
		}
	}
	return out, nil
}

// peekCached checks presence without recording a hit or promoting tiers.
func (bs *BlockStore) peekCached(k Key, blockIdx int64) ([]byte, bool) {
	ck := bs.cacheKey(k, blockIdx)
	if data, ok := bs.mem.Get(ck); ok {
		return data, true
	}
	return nil, false
}

// fetchRun fetches blocks [firstBlk, lastBlk] in one coalesced GET, verifies
// the ETag, stores each block, and returns them. Concurrent identical runs are
// joined via singleflight; single-block runs also join with prefetch.
func (bs *BlockStore) fetchRun(ctx context.Context, k Key, firstBlk, lastBlk, objSize int64, isPrefetch bool) ([][]byte, error) {
	flightKey := fmt.Sprintf("%s|%016x|%d-%d", k.Key, k.ETagHash, firstBlk, lastBlk)

	// Acquire concurrency slots (prefetch first takes a sub-limited slot so it
	// cannot consume all demand capacity).
	if isPrefetch {
		select {
		case bs.prefetch <- struct{}{}:
			defer func() { <-bs.prefetch }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	acquire := func() error {
		select {
		case bs.sem <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	release := func() { <-bs.sem }

	off := firstBlk * bs.blockSize
	length := (lastBlk - firstBlk + 1) * bs.blockSize
	if off+length > objSize {
		length = objSize - off
	}

	data, err, _ := bs.flight.Do(flightKey, func() ([]byte, error) {
		if aerr := acquire(); aerr != nil {
			return nil, aerr
		}
		defer release()
		buf, etag, gerr := bs.src.GetRange(ctx, k.Key, off, length)
		bs.record(func(r Recorder) { r.S3Get(int64(len(buf)), gerr != nil) })
		if gerr != nil {
			return nil, gerr
		}
		if xxh3.HashString(etag) != k.ETagHash {
			bs.markStale(k.Key)
			return nil, ErrStale
		}
		return buf, nil
	})
	if err != nil {
		return nil, err
	}

	// Split the coalesced buffer back into blocks and cache them.
	n := int(lastBlk - firstBlk + 1)
	blocks := make([][]byte, n)
	for b := 0; b < n; b++ {
		lo := int64(b) * bs.blockSize
		hi := lo + bs.blockSize
		if hi > int64(len(data)) {
			hi = int64(len(data))
		}
		if lo > int64(len(data)) {
			lo = int64(len(data))
		}
		blk := data[lo:hi]
		blocks[b] = blk
		bs.storeBlock(k, firstBlk+int64(b), blk)
	}
	return blocks, nil
}

func (bs *BlockStore) markStale(key string) {
	bs.mu.Lock()
	bs.stale[key] = struct{}{}
	bs.mu.Unlock()
	bs.record(func(r Recorder) { r.StaleKey(key) })
}

// StaleKeys returns the keys whose ETag no longer matched the index.
func (bs *BlockStore) StaleKeys() []string {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	out := make([]string, 0, len(bs.stale))
	for k := range bs.stale {
		out = append(out, k)
	}
	return out
}

func (bs *BlockStore) record(fn func(Recorder)) {
	if bs.rec != nil {
		fn(bs.rec)
	}
}
