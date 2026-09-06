// SPDX-License-Identifier: Apache-2.0

// Package blockstore implements lith's read-only cache over S3 range GETs. The
// cache unit is a fixed 1 MiB chunk; fills (and readahead) are done in
// block-sized runs of chunks and coalesced into a single range GET. A chunk
// singleflight keyed by (key, etagHash, chunkIdx) de-duplicates concurrent
// fetches: a range fill registers every chunk it will produce as in-flight
// before dispatching the GET and completes each chunk's waiters as its bytes
// arrive, so a demand read for an in-flight chunk joins it rather than issuing
// a second GET. See the pinned Design issue, §4.2, and issues #35 and #37.
package blockstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/zeebo/xxh3"
)

// ChunkSize is the fixed cache unit. Fills are multiples of it.
const ChunkSize int64 = 1 << 20 // 1 MiB

// diskFormat namespaces the on-disk cache layout; bumping it makes caches from
// an older layout (e.g. session 2's block-indexed files) be ignored.
const diskFormat = "chunkv1"

// ErrStale is returned when a fetched object's ETag does not match the index;
// the FUSE layer maps it to EIO and the key is recorded in the stale set.
var ErrStale = errors.New("blockstore: object changed since index build (ETag mismatch)")

// Source streams a byte range and reports the object's ETag. Satisfied by
// s3client and the test fake.
type Source interface {
	GetRangeReader(ctx context.Context, key string, off, length int64) (body io.ReadCloser, etag string, err error)
}

// Recorder receives cache and S3 events for metrics. A nil Recorder is fine.
type Recorder interface {
	MemHit()
	DiskHit()
	Miss()
	S3Get(bytes int64, isErr bool)
	StartInflight()
	EndInflight()
	StaleKey(key string)
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
	BlockSize     int64 // fill/readahead unit (default 8 MiB); rounded to a multiple of ChunkSize
	MemCache      int64
	DiskCache     int64
	DiskPath      string
	MaxRange      int64
	S3Concurrency int
	Recorder      Recorder
}

// chunkState is an in-flight (or just-completed) chunk fetch.
type chunkState struct {
	done chan struct{}
	data []byte
	err  error
}

// BlockStore is a read-only, chunk-granular tiered cache.
type BlockStore struct {
	src         Source
	bucket      string
	blockChunks int64 // BlockSize / ChunkSize
	maxChunks   int64 // MaxRange / ChunkSize

	mem  *mem2Q
	disk *diskTier

	sem      chan struct{} // total S3 concurrency
	prefetch chan struct{} // sub-limit so prefetch cannot starve demand

	rec Recorder

	mu         sync.Mutex
	inflight   map[string]*chunkState
	stale      map[string]struct{}
	prefetched map[string]struct{}
}

// New builds a BlockStore over src.
func New(src Source, cfg Config) (*BlockStore, error) {
	block := cfg.BlockSize
	if block <= 0 {
		block = 8 << 20
	}
	blockChunks := block / ChunkSize
	if blockChunks < 1 {
		blockChunks = 1
	}
	maxChunks := cfg.MaxRange / ChunkSize
	if maxChunks < blockChunks {
		maxChunks = blockChunks
	}
	conc := cfg.S3Concurrency
	if conc <= 0 {
		conc = 64
	}
	var disk *diskTier
	if cfg.DiskCache > 0 {
		d, err := newDiskTier(filepath.Join(cfg.DiskPath, diskFormat), cfg.DiskCache)
		if err != nil {
			return nil, err
		}
		disk = d
	}
	return &BlockStore{
		src:         src,
		bucket:      cfg.Bucket,
		blockChunks: blockChunks,
		maxChunks:   maxChunks,
		mem:         newMem2Q(cfg.MemCache),
		disk:        disk,
		sem:         make(chan struct{}, conc),
		prefetch:    make(chan struct{}, max(1, conc/2)),
		rec:         cfg.Recorder,
		inflight:    make(map[string]*chunkState),
		stale:       make(map[string]struct{}),
		prefetched:  make(map[string]struct{}),
	}, nil
}

// BlockSize returns the fill/readahead unit in bytes.
func (bs *BlockStore) BlockSize() int64 { return bs.blockChunks * ChunkSize }

// BlockChunks returns the number of chunks in a fill block.
func (bs *BlockStore) BlockChunks() int64 { return bs.blockChunks }

func (bs *BlockStore) cacheKey(k Key, chunkIdx int64) string {
	return fmt.Sprintf("%s|%s|%016x|%d", bs.bucket, k.Key, k.ETagHash, chunkIdx)
}

// lookup returns a cached chunk without recording metrics; tier is "mem",
// "disk", or "" (miss). A disk hit is promoted to memory.
func (bs *BlockStore) lookup(k Key, ci int64) ([]byte, string) {
	ck := bs.cacheKey(k, ci)
	if d, ok := bs.mem.Get(ck); ok {
		return d, "mem"
	}
	if d, ok := bs.disk.Get(ck); ok {
		bs.mem.Put(ck, d)
		return d, "disk"
	}
	return nil, ""
}

func (bs *BlockStore) storeChunk(k Key, ci int64, data []byte) {
	ck := bs.cacheKey(k, ci)
	bs.mem.Put(ck, data)
	bs.disk.Put(ck, data)
}

// claim registers chunk ci as in-flight. It returns the chunk's state and
// whether this caller owns the fetch (mine); if the chunk is already cached in
// memory it returns cachedData with cached=true; if a fetch is already in
// flight it returns that state with mine=false (the caller should join it).
func (bs *BlockStore) claim(k Key, ci int64) (cs *chunkState, mine bool, cachedData []byte, cached bool) {
	ck := bs.cacheKey(k, ci)
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if d, ok := bs.mem.Get(ck); ok {
		return nil, false, d, true
	}
	if existing, ok := bs.inflight[ck]; ok {
		return existing, false, nil, false
	}
	cs = &chunkState{done: make(chan struct{})}
	bs.inflight[ck] = cs
	return cs, true, nil, false
}

// complete stores the chunk, wakes its waiters, and clears its in-flight entry.
func (bs *BlockStore) complete(k Key, ci int64, cs *chunkState, data []byte, err error) {
	if err == nil {
		bs.storeChunk(k, ci, data)
	}
	cs.data, cs.err = data, err
	close(cs.done)
	ck := bs.cacheKey(k, ci)
	bs.mu.Lock()
	delete(bs.inflight, ck)
	bs.mu.Unlock()
}

// ensureChunks guarantees chunks [c0, c1] are cached (or returns the first
// error). It owns and fetches maximal contiguous runs of not-cached,
// not-in-flight chunks (coalesced into one GET, bounded by maxChunks) and joins
// chunks already in flight.
func (bs *BlockStore) ensureChunks(ctx context.Context, k Key, c0, c1, objSize int64, isPrefetch bool) error {
	i := c0
	for i <= c1 {
		if _, tier := bs.lookup(k, i); tier != "" {
			i++
			continue
		}
		cs, mine, _, cached := bs.claim(k, i)
		if cached {
			i++
			continue
		}
		if !mine {
			<-cs.done
			if cs.err != nil {
				return cs.err
			}
			i++
			continue
		}
		// We own chunk i; extend the run over contiguous chunks we also own.
		owned := []*chunkState{cs}
		j := i
		for j+1 <= c1 && int64(len(owned)) < bs.maxChunks {
			if _, tier := bs.lookup(k, j+1); tier != "" {
				break
			}
			cs2, mine2, _, cached2 := bs.claim(k, j+1)
			if cached2 || !mine2 {
				break
			}
			owned = append(owned, cs2)
			j++
		}
		if err := bs.fillRun(ctx, k, i, j, objSize, isPrefetch, owned); err != nil {
			return err
		}
		i = j + 1
	}
	return nil
}

// fillRun fetches chunks [first, last] in one streamed range GET, verifies the
// ETag, and completes each chunk's waiters as its bytes arrive.
func (bs *BlockStore) fillRun(ctx context.Context, k Key, first, last, objSize int64, isPrefetch bool, owned []*chunkState) error {
	// Acquire concurrency (prefetch takes a sub-limited slot first so it cannot
	// consume all demand capacity).
	if isPrefetch {
		select {
		case bs.prefetch <- struct{}{}:
			defer func() { <-bs.prefetch }()
		case <-ctx.Done():
			bs.failRun(k, first, owned, ctx.Err())
			return ctx.Err()
		}
	}
	select {
	case bs.sem <- struct{}{}:
		defer func() { <-bs.sem }()
	case <-ctx.Done():
		bs.failRun(k, first, owned, ctx.Err())
		return ctx.Err()
	}

	off := first * ChunkSize
	end := (last + 1) * ChunkSize
	if end > objSize {
		end = objSize
	}
	length := end - off

	bs.record(func(r Recorder) { r.StartInflight() })
	body, etag, err := bs.src.GetRangeReader(ctx, k.Key, off, length)
	bs.record(func(r Recorder) { r.S3Get(length, err != nil); r.EndInflight() })
	if err != nil {
		bs.failRun(k, first, owned, err)
		return err
	}
	defer func() { _ = body.Close() }()

	if xxh3.HashString(etag) != k.ETagHash {
		bs.markStale(k.Key)
		bs.failRun(k, first, owned, ErrStale)
		return ErrStale
	}

	for idx := range owned {
		ci := first + int64(idx)
		want := ChunkSize
		if ci*ChunkSize+want > objSize {
			want = objSize - ci*ChunkSize
		}
		if want < 0 {
			want = 0
		}
		buf := make([]byte, want)
		if _, rerr := io.ReadFull(body, buf); rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			// Fail this and every remaining chunk in the run.
			for r := idx; r < len(owned); r++ {
				bs.complete(k, first+int64(r), owned[r], nil, rerr)
			}
			return rerr
		}
		bs.complete(k, ci, owned[idx], buf, nil)
	}
	return nil
}

// failRun completes every owned chunk in a run with err.
func (bs *BlockStore) failRun(k Key, first int64, owned []*chunkState, err error) {
	for idx := range owned {
		bs.complete(k, first+int64(idx), owned[idx], nil, err)
	}
}

// GetRange returns bytes for [off, off+length) of the object, fetching only the
// chunks the read spans (contiguous misses coalesced into one GET). This is the
// demand path: a small read fetches a single 1 MiB chunk.
func (bs *BlockStore) GetRange(ctx context.Context, k Key, off, length, objSize int64) ([]byte, error) {
	if off >= objSize || length <= 0 {
		return []byte{}, nil
	}
	end := off + length
	if end > objSize {
		end = objSize
	}
	c0 := off / ChunkSize
	c1 := (end - 1) / ChunkSize

	// Record hit/miss per chunk and credit prefetch accuracy.
	for ci := c0; ci <= c1; ci++ {
		if _, tier := bs.lookup(k, ci); tier != "" {
			switch tier {
			case "mem":
				bs.record(func(r Recorder) { r.MemHit() })
			case "disk":
				bs.record(func(r Recorder) { r.DiskHit() })
			}
			bs.notePrefetchHit(bs.cacheKey(k, ci))
		} else {
			bs.record(func(r Recorder) { r.Miss() })
		}
	}

	if err := bs.ensureChunks(ctx, k, c0, c1, objSize, false); err != nil {
		return nil, err
	}

	out := make([]byte, 0, end-off)
	for ci := c0; ci <= c1; ci++ {
		data, tier := bs.lookup(k, ci)
		if tier == "" {
			return nil, fmt.Errorf("blockstore: chunk %d missing after fill", ci)
		}
		lo := int64(0)
		start := ci * ChunkSize
		if off > start {
			lo = off - start
		}
		hi := int64(len(data))
		if start+hi > end {
			hi = end - start
		}
		if lo < hi {
			out = append(out, data[lo:hi]...)
		}
	}
	return out, nil
}

// Prefetch fills a whole block (blockChunks chunks) starting at blockIdx, at
// prefetch priority. Errors are swallowed (best-effort).
func (bs *BlockStore) Prefetch(ctx context.Context, k Key, blockIdx, objSize int64) {
	c0 := blockIdx * bs.blockChunks
	if c0*ChunkSize >= objSize {
		return
	}
	c1 := c0 + bs.blockChunks - 1
	lastChunk := (objSize - 1) / ChunkSize
	if c1 > lastChunk {
		c1 = lastChunk
	}
	// Mark not-yet-cached chunks as prefetched for accuracy accounting.
	for ci := c0; ci <= c1; ci++ {
		if _, tier := bs.lookup(k, ci); tier == "" {
			ck := bs.cacheKey(k, ci)
			bs.mu.Lock()
			if _, dup := bs.prefetched[ck]; !dup {
				bs.prefetched[ck] = struct{}{}
				bs.mu.Unlock()
				bs.record(func(r Recorder) { r.PrefetchIssued() })
			} else {
				bs.mu.Unlock()
			}
		}
	}
	_ = bs.ensureChunks(ctx, k, c0, c1, objSize, true)
}

// notePrefetchHit credits a prefetch the first time a demand read consumes a
// chunk that prefetch had fetched.
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
