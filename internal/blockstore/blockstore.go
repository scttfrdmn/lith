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
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scttfrdmn/lith/internal/cargoship"
	"github.com/zeebo/xxh3"
)

// ChunkSize is the fixed cache unit. Fills are multiples of it.
const ChunkSize int64 = 1 << 20 // 1 MiB

// diskFormat namespaces the on-disk cache layout; bumping it makes caches from
// an older layout (e.g. session 2's block-indexed files) be ignored.
const diskFormat = "chunkv2" // v2: each file is [2-byte LE filled-extent bitmap][chunk bytes] (#118)

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
	// UncoveredMiss is a demand read whose chunk was neither cached nor in
	// flight, so the demand read had to originate the fetch itself — a sign the
	// readahead frontier did not lead far enough (#38).
	UncoveredMiss()
}

// Key identifies an object plus the ETag hash recorded in the index. For a
// CargoShip-backed virtual file, Key names the packed `.tar.zst` chunk object,
// ETagHash is that object's ETag, and Cargo carries the chunk's frame table so
// a read of the chunk's uncompressed byte space maps to zstd frame range GETs
// (#94). Cargo is nil for an ordinary object (a plain key → range GET).
type Key struct {
	Key      string
	ETagHash uint64
	Cargo    *CargoChunk
}

// CargoChunk is the frame table of one packed `.tar.zst` chunk plus its total
// uncompressed tar-stream size. It travels on a Key so the block store can
// decode a chunk's uncompressed byte range from the covering zstd frames.
type CargoChunk struct {
	Frames      []cargoship.Frame
	UncompTotal int64
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
	// PrefetchConcurrency caps concurrent prefetch fills. <=0 defaults to
	// S3Concurrency (prefetch may use the whole request budget). A value below
	// S3Concurrency reserves the remainder for demand fills so prefetch cannot
	// starve them.
	PrefetchConcurrency int
	DiskWriters         int   // write-behind workers for the disk tier (default 4)
	InflightBytes       int64 // bytes-in-flight budget (0 disables byte gating)
	// PrefetchBudget caps the bytes held for prefetch not yet demanded (in-flight
	// + fetched-unread), so aggregate readahead cannot thrash the memory tier
	// (#55). <=0 defaults to 50% of MemCache.
	PrefetchBudget int64
	// CoalesceGap, when > 0, is an explicit override of the largest gap (bytes)
	// between two plan/demand fill ranges that FillBatch merges into one range GET
	// (#124). When 0, the gap is derived from the device: NICBytesPerSec × TTFB,
	// clamped to [256 KiB, 64 MiB] (#124/session 30) — a round-trip's worth of
	// streaming, so on a fat pipe lith merges freely and on a small one it stays
	// byte-precise.
	CoalesceGap int64
	// NICBytesPerSec is the NIC baseline bandwidth used to derive the coalesce gap
	// (0 disables derivation → the 256 KiB floor).
	NICBytesPerSec int64
	// TTFB seeds the rolling first-byte-latency median used to derive the gap
	// before any fill has been measured (e.g. from the index-build HEADs).
	TTFB     time.Duration
	Recorder Recorder
}

// chunkState is an in-flight (or just-completed) chunk fetch. With sparse fills
// (#118) a fetch may fill only some extents; filled is the bitmap the completed
// fetch produced, so a joiner can tell whether its wanted extents are covered or
// it must fetch the remainder. base{Data,Filled} carry any partial chunk the
// owner is extending (copy-on-merge).
type chunkState struct {
	done       chan struct{}
	data       []byte
	filled     uint16
	err        error
	baseData   []byte
	baseFilled uint16
}

// BlockStore is a read-only, chunk-granular tiered cache.
type BlockStore struct {
	src         Source
	bucket      string
	blockChunks int64 // BlockSize / ChunkSize
	maxChunks   int64 // MaxRange / ChunkSize

	mem  *memCache
	disk *diskTier

	sem      chan struct{} // total S3 request-count hard cap
	prefetch chan struct{} // cap on concurrent prefetch fills (<= sem)
	budget   *bytesBudget  // bytes-in-flight budget (nil = disabled)
	// pfBudgetBytes bounds aggregate un-demanded prefetch; the FUSE layer turns
	// it into a per-handle window = pfBudgetBytes/handles (#55). 0 disables.
	pfBudgetBytes int64
	// Coalesce-gap derivation (#124/session 30). gapOverride > 0 pins the gap;
	// otherwise it is nicBPS × the rolling TTFB median, clamped. ttfb is guarded
	// by ttfbMu: the first ttfbMax fill first-byte latencies (seeded by ttfbSeed).
	gapOverride  int64
	nicBPS       int64
	prefetchConc int          // usable prefetch concurrency, for the concurrency-aware gap (#31)
	fillInflight atomic.Int64 // fill-batch runs currently fetching (#31)
	fillPeak     atomic.Int64 // high-water mark of fillInflight (for tests)
	ttfbMu       sync.Mutex
	ttfbSeed     time.Duration
	ttfbSamples  []time.Duration

	// Demand batching (#124/session 30): concurrent footer demand misses on one
	// object are collected for one scheduling tick and dispatched as a single
	// coalesced FillBatch, so a front-loaded read burst becomes a few range GETs.
	dmu    sync.Mutex
	demand map[string]*demandGather

	rec Recorder
	// timeline is the optional per-chunk diagnostic sink (#70); nil unless the
	// installed Recorder implements chunkTimelineRecorder. When nil the read
	// path takes no extra clock reads or counter updates.
	timeline  chunkTimelineRecorder
	fill      fillRecorder    // optional sparse-fill metrics sink (#118); nil when unimplemented
	backing   backingRecorder // optional CargoShip backing metrics sink (#94); nil when unimplemented
	zdec      *zstd.Decoder   // shared zstd frame decoder (DecodeAll is concurrency-safe); nil until first cargoship fill
	zdecOnce  sync.Once
	inflightN atomic.Int64 // chunk fetches currently in flight (maintained only when timeline != nil)

	mu       sync.Mutex
	inflight map[string]*chunkState
	stale    map[string]struct{}
	// prefetched holds chunk keys fetched by prefetch and not yet demanded; a
	// sync.Map so the mem-tier eviction callback can release the budget without
	// taking bs.mu under the shard lock (avoids a lock-order inversion).
	prefetched sync.Map

	// Write-behind disk tier: prefetch fills apply backpressure on this queue
	// (bounded), demand fills never block; shutdown drains via stop.
	diskWrites    chan diskWriteReq
	stop          chan struct{}
	writersWG     sync.WaitGroup
	closeOnce     sync.Once
	pendingWrites atomic.Int64
}

type diskWriteReq struct {
	cacheKey string
	data     []byte
	filled   uint16
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
	prefetchConc := cfg.PrefetchConcurrency
	if prefetchConc <= 0 {
		prefetchConc = conc
	}
	if prefetchConc > conc {
		prefetchConc = conc
	}
	var disk *diskTier
	if cfg.DiskCache > 0 {
		d, err := newDiskTier(filepath.Join(cfg.DiskPath, diskFormat), cfg.DiskCache)
		if err != nil {
			return nil, err
		}
		disk = d
	}
	pfBudgetCap := cfg.PrefetchBudget
	if pfBudgetCap <= 0 {
		pfBudgetCap = cfg.MemCache / 2
	}
	bs := &BlockStore{
		src:           src,
		bucket:        cfg.Bucket,
		blockChunks:   blockChunks,
		maxChunks:     maxChunks,
		mem:           newMemCache(cfg.MemCache, 64),
		disk:          disk,
		sem:           make(chan struct{}, conc),
		prefetch:      make(chan struct{}, max(1, prefetchConc)),
		budget:        newBytesBudget(cfg.InflightBytes),
		pfBudgetBytes: pfBudgetCap,
		gapOverride:   cfg.CoalesceGap,
		nicBPS:        cfg.NICBytesPerSec,
		prefetchConc:  prefetchConc,
		ttfbSeed:      cfg.TTFB,
		rec:           cfg.Recorder,
		inflight:      make(map[string]*chunkState),
		stale:         make(map[string]struct{}),
		demand:        make(map[string]*demandGather),
	}
	if r, ok := cfg.Recorder.(chunkTimelineRecorder); ok {
		bs.timeline = r
	}
	if r, ok := cfg.Recorder.(fillRecorder); ok {
		bs.fill = r
	}
	if r, ok := cfg.Recorder.(backingRecorder); ok {
		bs.backing = r
	}
	bs.mem.setOnEvictUnread(bs.onEvictUnread)
	if disk != nil {
		writers := cfg.DiskWriters
		if writers <= 0 {
			writers = 4
		}
		bs.diskWrites = make(chan diskWriteReq, writers*64)
		bs.stop = make(chan struct{})
		for i := 0; i < writers; i++ {
			bs.writersWG.Add(1)
			go bs.diskWriter()
		}
	}
	return bs, nil
}

// diskWriter drains queued disk writes off the fill path until stopped, then
// drains anything remaining in the queue.
func (bs *BlockStore) diskWriter() {
	defer bs.writersWG.Done()
	for {
		select {
		case req := <-bs.diskWrites:
			bs.writeOne(req)
		case <-bs.stop:
			for {
				select {
				case req := <-bs.diskWrites:
					bs.writeOne(req)
				default:
					return
				}
			}
		}
	}
}

// Flush blocks until all queued disk writes have been persisted. Useful before
// asserting disk-tier state and to guarantee the cache is durable at unmount.
func (bs *BlockStore) Flush() {
	for bs.pendingWrites.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
}

// Close stops the write-behind workers and waits for pending writes. Safe to
// call once per store (e.g. at unmount).
func (bs *BlockStore) Close() {
	bs.closeOnce.Do(func() {
		if bs.diskWrites != nil {
			bs.Flush() // drain pending writes (durable cache) while workers run
			close(bs.stop)
			bs.writersWG.Wait()
		}
		if bs.zdec != nil {
			bs.zdec.Close()
		}
	})
}

// QueueDepth returns the number of chunks queued for the write-behind disk
// writer (0 when the disk tier is disabled).
func (bs *BlockStore) QueueDepth() int {
	if bs.diskWrites == nil {
		return 0
	}
	return len(bs.diskWrites)
}

// BlockSize returns the fill/readahead unit in bytes.
func (bs *BlockStore) BlockSize() int64 { return bs.blockChunks * ChunkSize }

// BlockChunks returns the number of chunks in a fill block.
func (bs *BlockStore) BlockChunks() int64 { return bs.blockChunks }

func (bs *BlockStore) cacheKey(k Key, chunkIdx int64) string {
	return fmt.Sprintf("%s|%s|%016x|%d", bs.bucket, k.Key, k.ETagHash, chunkIdx)
}

// lookup returns a cached chunk with its filled-extent bitmap (#118) without
// recording metrics; tier is "mem", "disk", or "" (miss). A disk hit is promoted
// to memory. A chunk may be partially filled: the caller checks coverage of the
// extents it needs against filled.
func (bs *BlockStore) lookup(k Key, ci int64) (data []byte, filled uint16, tier string) {
	ck := bs.cacheKey(k, ci)
	if d, f, ok := bs.mem.Get(ck); ok {
		return d, f, "mem"
	}
	if d, f, ok := bs.disk.Get(ck); ok {
		bs.mem.Put(ck, d, f)
		return d, f, "disk"
	}
	return nil, 0, ""
}

// enqueueDiskWrite hands a chunk to the write-behind pool. A prefetch fill
// blocks on a full queue (backpressure — prefetch should not outrun the disk),
// keeping the pipeline bounded; a demand fill never blocks (it drops the disk
// write on a full queue rather than delay the reader). The chunk is pinned in
// memory until its write lands so it is not evicted and re-fetched.
func (bs *BlockStore) enqueueDiskWrite(ck string, data []byte, filled uint16, blocking bool) {
	if bs.diskWrites == nil {
		return
	}
	bs.mem.Pin(ck)
	bs.pendingWrites.Add(1)
	req := diskWriteReq{cacheKey: ck, data: data, filled: filled}
	if blocking {
		select {
		case bs.diskWrites <- req:
		case <-bs.stop:
			bs.writeOne(req) // shutting down: write inline so nothing is lost
		}
		return
	}
	select {
	case bs.diskWrites <- req:
	case <-bs.stop:
		bs.writeOne(req)
	default:
		bs.pendingWrites.Add(-1) // demand fill, queue full: skip disk
		bs.mem.Unpin(ck)
	}
}

// writeOne persists a single chunk and releases its pin.
func (bs *BlockStore) writeOne(req diskWriteReq) {
	bs.disk.Put(req.cacheKey, req.data, req.filled)
	bs.mem.Unpin(req.cacheKey)
	bs.pendingWrites.Add(-1)
}

// claim registers chunk ci as in-flight for the extents in want (#118).
// Singleflight is keyed per chunk: at most one fetch per chunk is in flight, and
// a fill for some extents joins/extends it rather than racing it. It returns:
//   - cached=true with cachedData/cachedFilled when the memory tier already
//     covers want (all wanted extents filled);
//   - mine=false + an existing chunkState to join when a fetch is already in
//     flight (the joiner waits, then re-checks coverage — the in-flight fill may
//     cover fewer extents than the joiner needs, in which case it re-claims);
//   - mine=true + a fresh chunkState carrying any partial chunk as base{Data,
//     Filled} for the owner to extend (copy-on-merge).
func (bs *BlockStore) claim(k Key, ci int64, want uint16) (cs *chunkState, mine bool, cachedData []byte, cachedFilled uint16, cached bool) {
	ck := bs.cacheKey(k, ci)
	bs.mu.Lock()
	defer bs.mu.Unlock()
	base, baseFilled, _ := bs.memGetLocked(ck)
	if base != nil && covers(baseFilled, want) {
		return nil, false, base, baseFilled, true
	}
	if existing, ok := bs.inflight[ck]; ok {
		return existing, false, nil, 0, false
	}
	cs = &chunkState{done: make(chan struct{}), baseData: base, baseFilled: baseFilled}
	bs.inflight[ck] = cs
	if bs.timeline != nil {
		bs.inflightN.Add(1)
	}
	return cs, true, nil, 0, false
}

// memGetLocked reads the memory tier only (no disk promotion), for use under
// bs.mu inside claim.
func (bs *BlockStore) memGetLocked(ck string) ([]byte, uint16, bool) {
	return bs.mem.Get(ck)
}

// complete wakes the chunk's waiters (with its bytes) and clears its in-flight
// entry, then hands the disk write to the write-behind pool. Waiters are woken
// before the (possibly blocking) disk enqueue so a joining demand read is never
// delayed by disk backpressure. blocking is true for prefetch fills.
func (bs *BlockStore) complete(k Key, ci int64, cs *chunkState, data []byte, filled uint16, err error, blocking bool) {
	ck := bs.cacheKey(k, ci)
	if err == nil {
		// Merge accumulates newly filled extents into any partial chunk already
		// cached (#118); the merged buffer is complete and immutable. blocking ==
		// prefetch fill: also flag it unread (evicted last) so it holds a budget
		// reservation until a demand read consumes it or it is evicted (#55).
		bs.mem.Merge(ck, data, filled)
		if blocking {
			bs.mem.MarkUnread(ck)
		}
	} else if blocking {
		// Prefetch fill failed: drop its prefetched marker.
		bs.prefetched.LoadAndDelete(ck)
	}
	cs.data, cs.filled, cs.err = data, filled, err
	close(cs.done)
	bs.mu.Lock()
	delete(bs.inflight, ck)
	bs.mu.Unlock()
	if bs.timeline != nil {
		bs.inflightN.Add(-1)
	}
	if err == nil {
		bs.enqueueDiskWrite(ck, data, filled, blocking)
	}
}

// ensureChunks guarantees chunks [c0, c1] hold the extents wantOf gives each,
// coalescing maximal contiguous runs of not-covered, not-in-flight chunks into
// one byte-exact range GET (bounded by maxChunks) and joining chunks already in
// flight. wantOf lets a whole-chunk consumer (Prefetch) ask for full chunks and
// a demand read (GetRange) ask for only the extents it touches (#118). When out
// is non-nil it is populated with each chunk's bytes, so a demand read assembles
// from the fetched data directly rather than a second cache lookup (which the
// async write-behind disk tier cannot guarantee is present yet).
func (bs *BlockStore) ensureChunks(ctx context.Context, k Key, c0, c1, objSize int64, isPrefetch bool, out map[int64][]byte, wantOf func(ci int64) uint16, kind fillKind) error {
	i := c0
	for i <= c1 {
		want := wantOf(i) & maskForLen(chunkLenOf(i, objSize))
		if want == 0 {
			i++
			continue
		}
		if d, f, tier := bs.lookup(k, i); tier != "" && covers(f, want) {
			if out != nil {
				out[i] = d
			}
			i++
			continue
		}
		cs, mine, cachedData, _, cached := bs.claim(k, i, want)
		if cached {
			if out != nil {
				out[i] = cachedData
			}
			i++
			continue
		}
		if !mine {
			var t0 time.Time
			var pf bool
			if bs.timeline != nil && !isPrefetch {
				t0 = time.Now()
				_, pf = bs.prefetched.Load(bs.cacheKey(k, i))
			}
			<-cs.done
			if cs.err != nil {
				return cs.err
			}
			if bs.timeline != nil && !isPrefetch {
				bs.emitChunk(k, i, "join", pf, time.Since(t0), t0)
			}
			data := cs.data
			// The in-flight fill we joined may have covered fewer extents than we
			// need; top this chunk up (#118).
			if !covers(cs.filled, want) {
				var err error
				if data, err = bs.fetchExtents(ctx, k, i, want, objSize, isPrefetch, kind); err != nil {
					return err
				}
			}
			if out != nil {
				out[i] = data
			}
			i++
			continue
		}
		// We own chunk i and must fetch it ourselves. For a demand read that
		// means the readahead frontier did not cover it.
		if !isPrefetch {
			bs.record(func(r Recorder) { r.UncoveredMiss() })
		}
		// Extend the run over contiguous chunks we also own and want, coalescing
		// their wanted extents into one GET.
		owned := []*chunkState{cs}
		wants := []uint16{want}
		j := i
		for j+1 <= c1 && int64(len(owned)) < bs.maxChunks {
			wantNext := wantOf(j+1) & maskForLen(chunkLenOf(j+1, objSize))
			if wantNext == 0 {
				break
			}
			if _, f, tier := bs.lookup(k, j+1); tier != "" && covers(f, wantNext) {
				break
			}
			cs2, mine2, _, _, cached2 := bs.claim(k, j+1, wantNext)
			if cached2 || !mine2 {
				break
			}
			owned = append(owned, cs2)
			wants = append(wants, wantNext)
			j++
		}
		if err := bs.fillRun(ctx, k, i, j, objSize, isPrefetch, owned, wants, kind); err != nil {
			return err
		}
		if out != nil {
			for idx := range owned {
				out[i+int64(idx)] = owned[idx].data
			}
		}
		i = j + 1
	}
	return nil
}

// fullWant is a wantOf that asks for every extent of each chunk (whole-chunk
// streaming/prefetch).
func (bs *BlockStore) fullWant(objSize int64) func(int64) uint16 {
	return func(ci int64) uint16 { return maskForLen(chunkLenOf(ci, objSize)) }
}

// fillRun fetches the wanted extents of chunks [first, last] in one streamed
// byte-exact range GET (from the first wanted extent of the first chunk through
// the last wanted extent of the last chunk), verifies the ETag, and completes
// each chunk's waiters with its merged buffer. For a full-chunk run this is the
// original whole-block fill; for a partial run it fetches only the extents the
// reads touch (#118).
func (bs *BlockStore) fillRun(ctx context.Context, k Key, first, last, objSize int64, isPrefetch bool, owned []*chunkState, wants []uint16, kind fillKind) error {
	// Acquire concurrency (prefetch takes a sub-limited slot first so it cannot
	// consume all demand capacity). Time the wait so the bimodality
	// investigation (#49) can see whether prefetch fills are starving on the
	// semaphore.
	if isPrefetch {
		waitStart := time.Now()
		select {
		case bs.prefetch <- struct{}{}:
			bs.recordPrefetchWait(time.Since(waitStart))
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

	// The byte span covering every chunk's wanted extents: first wanted extent of
	// the first chunk through the last wanted extent of the last chunk. For a
	// full-chunk run this is [first*ChunkSize, end-of-last]; for a partial run it
	// is only the touched extents. The run is contiguous (whole interior chunks
	// for a contiguous read / full masks), so the per-chunk wanted byte ranges
	// concatenate with no gaps and the body streams straight into each buffer.
	lo0, _ := extentByteRange(wants[0], chunkLenOf(first, objSize))
	off := first*ChunkSize + lo0
	var length int64
	for idx := range owned {
		cl := chunkLenOf(first+int64(idx), objSize)
		l, h := extentByteRange(wants[idx], cl)
		length += h - l
	}
	if length <= 0 {
		for idx := range owned {
			bs.complete(k, first+int64(idx), owned[idx], nil, 0, nil, isPrefetch)
		}
		return nil
	}

	if got := bs.budget.acquire(length); got > 0 {
		defer bs.budget.release(got)
	}

	body, etag, err := bs.fetchReader(ctx, k, off, length)
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
		cl := chunkLenOf(ci, objSize)
		lo, hi := extentByteRange(wants[idx], cl)
		// Seed with any partial chunk we're extending (copy-on-merge), then read
		// the wanted extents. A short read is a truncation, never a zero-padded
		// "success" (H1): fail this and every remaining chunk in the run.
		buf := cloneChunk(owned[idx].baseData, cl)
		if _, rerr := io.ReadFull(body, buf[lo:hi]); rerr != nil {
			for r := idx; r < len(owned); r++ {
				bs.complete(k, first+int64(r), owned[r], nil, 0, rerr, isPrefetch)
			}
			return rerr
		}
		bs.complete(k, ci, owned[idx], buf, owned[idx].baseFilled|wants[idx], nil, isPrefetch)
	}
	bs.recordFill(kind, length)
	return nil
}

// Chunk returns a chunk buffer for a demand read of [readLo, readHi) within
// chunk ci (byte offsets relative to the chunk). It guarantees the extents that
// read covers are filled and returns the whole-chunk buffer so the FUSE layer
// can hand a sub-slice straight to the kernel (no copy, no allocation on a hit).
// When sequential is set (streaming), the whole chunk is filled — streaming is
// unchanged; otherwise only the read's 64 KiB extents are fetched (#118), so a
// point read of a plan-prefetched column is served without pulling the rest of
// the 1 MiB chunk. The buffer is immutable; the runtime keeps it alive while the
// reply references it.
func (bs *BlockStore) Chunk(ctx context.Context, k Key, ci, objSize, readLo, readHi int64, sequential bool) ([]byte, error) {
	want := extentMask(readLo, readHi)
	if sequential {
		want = maskForLen(chunkLenOf(ci, objSize))
	}
	var t0 time.Time
	hit := false
	if bs.timeline != nil {
		t0 = time.Now()
		if _, f, tier := bs.lookup(k, ci); tier != "" && covers(f, want) {
			hit = true
		}
	}
	d, err := bs.fetchExtents(ctx, k, ci, want, objSize, false, fillDemand)
	if err != nil {
		return nil, err
	}
	if bs.timeline != nil {
		if hit {
			bs.emitChunk(k, ci, "hit", false, 0, t0)
		} else {
			bs.emitChunk(k, ci, "uncovered", false, time.Since(t0), t0)
		}
	}
	return d, nil
}

// failRun completes every owned chunk in a run with err.
func (bs *BlockStore) failRun(k Key, first int64, owned []*chunkState, err error) {
	for idx := range owned {
		bs.complete(k, first+int64(idx), owned[idx], nil, 0, err, false)
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

	// Extent-aware demand assembly (#118): each chunk wants only the extents it
	// contributes to [off,end), so a read served from plan-prefetched extents
	// adds no S3 bytes and a straddling read pulls only its own bytes — while
	// contiguous not-cached chunks still coalesce into one byte-exact GET.
	wantOf := func(ci int64) uint16 {
		start := ci * ChunkSize
		lo := int64(0)
		if off > start {
			lo = off - start
		}
		return extentMask(lo, end-start)
	}
	chunks := make(map[int64][]byte, c1-c0+1)
	if err := bs.ensureChunks(ctx, k, c0, c1, objSize, false, chunks, wantOf, fillDemand); err != nil {
		return nil, err
	}
	out := make([]byte, 0, end-off)
	for ci := c0; ci <= c1; ci++ {
		data := chunks[ci]
		lo := int64(0)
		start := ci * ChunkSize
		if off > start {
			lo = off - start
		}
		hi := end - start
		if hi > int64(len(data)) {
			hi = int64(len(data))
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
	// Mark not-yet-cached chunks as prefetched (unread) so a demand read can
	// credit the hit and eviction can prefer already-read chunks over them. The
	// aggregate readahead is bounded by the per-handle window the FUSE layer
	// derives from the prefetch budget and the live handle count (#55), so this
	// path does not gate.
	for ci := c0; ci <= c1; ci++ {
		if _, f, tier := bs.lookup(k, ci); tier != "" && covers(f, maskForLen(chunkLenOf(ci, objSize))) {
			continue
		}
		ck := bs.cacheKey(k, ci)
		if _, dup := bs.prefetched.LoadOrStore(ck, struct{}{}); !dup {
			bs.record(func(r Recorder) { r.PrefetchIssued() })
			if bs.timeline != nil {
				bs.timeline.PrefetchDispatch(k.Key, ci, time.Now())
			}
		}
	}
	_ = bs.ensureChunks(ctx, k, c0, c1, objSize, true, nil, bs.fullWant(objSize), fillWhole)
}

// PrefetchBudgetBytes is the mount-wide byte budget for prefetch not yet
// demanded. It is the total the prefetch Limits policy is built with (#64); the
// per-handle window and the sibling/parts reservations all draw on it.
func (bs *BlockStore) PrefetchBudgetBytes() int64 { return bs.pfBudgetBytes }

// PrefetchBudgetBlocks is the number of readahead blocks the prefetch budget
// allows to be outstanding across all handles; the FUSE layer divides it by the
// live handle count to size each handle's window (#55).
func (bs *BlockStore) PrefetchBudgetBlocks() int64 {
	if bs.pfBudgetBytes <= 0 || bs.blockChunks <= 0 {
		return 0
	}
	n := bs.pfBudgetBytes / (bs.blockChunks * ChunkSize)
	if n < 1 {
		n = 1
	}
	return n
}

// onEvictUnread is invoked by the memory tier when a prefetched-but-unread chunk
// is evicted — the thrash signal. It releases the chunk's prefetch reservation.
// Called under a shard lock, so it must stay lock-free w.r.t. bs.mu (hence the
// sync.Map).
func (bs *BlockStore) onEvictUnread(ck string) {
	if _, ok := bs.prefetched.LoadAndDelete(ck); ok {
		bs.recordPrefetchEvicted()
	}
}

// notePrefetchHit credits a prefetch the first time a demand read consumes a
// chunk that prefetch had fetched, clears its unread flag, and releases its
// prefetch-budget reservation.
func (bs *BlockStore) notePrefetchHit(ck string) {
	if _, ok := bs.prefetched.LoadAndDelete(ck); ok {
		bs.mem.ClearUnread(ck)
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

// ChunkEvent is one demanded-chunk read observed for the #70 timeline
// diagnostic. Kind is "hit" (served from cache), "join" (blocked on an
// in-flight fill), or "uncovered" (the demand read originated the fetch). Wait
// is the time from read entry to data-ready (0 for a hit; the join wait for a
// join; the self-fill duration for an uncovered miss). Inflight is the number
// of chunk fetches in flight at read entry.
type ChunkEvent struct {
	At         time.Time
	Key        string
	Chunk      int64
	Kind       string
	Prefetched bool
	Wait       time.Duration
	Inflight   int
}

// chunkTimelineRecorder is an optional Recorder extension: implementers receive
// a per-demand-chunk timeline and the dispatch time of each prefetch, so the
// join-wait diagnostic (#70) can correlate prefetch dispatch with app open.
// Optional (type assertion) so existing Recorder implementers need not change;
// when the installed Recorder does not implement it, bs.timeline is nil and the
// read path is unchanged.
type chunkTimelineRecorder interface {
	PrefetchDispatch(key string, chunk int64, at time.Time)
	ChunkRead(ev ChunkEvent)
}

// emitChunk records one demand-chunk timeline event (only called when
// bs.timeline != nil).
func (bs *BlockStore) emitChunk(k Key, ci int64, kind string, prefetched bool, wait time.Duration, at time.Time) {
	bs.timeline.ChunkRead(ChunkEvent{
		At: at, Key: k.Key, Chunk: ci, Kind: kind,
		Prefetched: prefetched, Wait: wait, Inflight: int(bs.inflightN.Load()),
	})
}

// prefetchWaitRecorder is an optional Recorder extension: implementers receive
// the time each prefetch fill spent blocked acquiring the prefetch semaphore.
// Kept optional (type assertion) so existing Recorder implementers need not
// change.
type prefetchWaitRecorder interface {
	PrefetchWait(d time.Duration)
}

func (bs *BlockStore) recordPrefetchWait(d time.Duration) {
	if r, ok := bs.rec.(prefetchWaitRecorder); ok {
		r.PrefetchWait(d)
	}
}

// prefetchEvictRecorder is an optional Recorder extension: implementers are
// told when a prefetched-but-unread chunk was evicted (the #55 thrash signal).
type prefetchEvictRecorder interface {
	PrefetchEvictedUnread()
}

func (bs *BlockStore) recordPrefetchEvicted() {
	if r, ok := bs.rec.(prefetchEvictRecorder); ok {
		r.PrefetchEvictedUnread()
	}
}

// fillRecorder is an optional Recorder extension for the sparse-fill metrics
// (#118/#124): partial fills, bytes by fill kind, coalesced-run count, and bytes
// fetched only to close gaps.
type fillRecorder interface {
	FillPartial()
	FillBytes(kind string, n int64)
	FillRun()
	FillGapBytes(n int64)
	FillBatchSize(n int)
	FillInflight(delta float64)
	FillInflightPeak(n float64)
}

func (bs *BlockStore) recordFill(kind fillKind, n int64) {
	if bs.fill == nil || kind == fillSilent {
		return
	}
	if kind != fillWhole {
		bs.fill.FillPartial()
	}
	bs.fill.FillBytes(kind.label(), n)
}

// recordBatch records a coalesced fill batch (#124): plan bytes (the requested
// projection), gap bytes (fetched only to close sub-coalesceGap gaps), and one
// FillRun per merged range GET.
func (bs *BlockStore) recordBatch(planBytes, gapBytes, runs int64, batchSize int) {
	if bs.fill == nil {
		return
	}
	bs.fill.FillBatchSize(batchSize)
	if planBytes > 0 {
		bs.fill.FillPartial()
		bs.fill.FillBytes("plan", planBytes)
	}
	if gapBytes > 0 {
		bs.fill.FillBytes("gap", gapBytes)
		bs.fill.FillGapBytes(gapBytes)
	}
	for i := int64(0); i < runs; i++ {
		bs.fill.FillRun()
	}
}
