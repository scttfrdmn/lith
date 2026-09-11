// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"io"
	"sort"

	"github.com/zeebo/xxh3"
)

// Sparse chunk fills (#118). fetchExtents is the extent-aware demand/plan fill:
// it guarantees a chunk holds the extents a caller needs, fetching only the
// missing extents byte-exact rather than the whole 1 MiB chunk. Whole-chunk
// streaming (Prefetch → ensureChunks → fillRun) is untouched and still coalesces
// runs into block-sized GETs; the two lanes share the per-chunk singleflight
// (claim/complete), so an extent fill and a whole-chunk fill for the same chunk
// join into one GET rather than racing.

// fillKind labels a fill for the lith_fill_bytes_total{kind} metric.
type fillKind int

const (
	fillWhole  fillKind = iota // a whole-chunk fill (streaming/prefetch/sequential demand)
	fillDemand                 // a demand read of an unfilled extent (non-sequential)
	fillPlan                   // a format plan's byte-exact projection range
	fillSilent                 // a fill whose bytes FillBatch accounts itself (no per-run recordFill)
)

func (fk fillKind) label() string {
	switch fk {
	case fillDemand:
		return "demand"
	case fillPlan:
		return "plan"
	default:
		return "whole"
	}
}

// chunkLenOf is the byte length of chunk ci (ChunkSize, short for the last one).
func chunkLenOf(ci, objSize int64) int64 {
	l := objSize - ci*ChunkSize
	if l > ChunkSize {
		l = ChunkSize
	}
	if l < 0 {
		l = 0
	}
	return l
}

// fetchExtents ensures chunk ci has every extent in want filled and returns the
// chunk buffer (whole ChunkSize, unfilled extents zero). Only the missing
// extents are GET-ed. It records the hit/miss and prefetch-accuracy metrics the
// demand path expects.
func (bs *BlockStore) fetchExtents(ctx context.Context, k Key, ci int64, want uint16, objSize int64, isPrefetch bool, kind fillKind) ([]byte, error) {
	cl := chunkLenOf(ci, objSize)
	want &= maskForLen(cl)
	if want == 0 {
		return []byte{}, nil
	}
	for {
		if d, f, tier := bs.lookup(k, ci); tier != "" && covers(f, want) {
			switch tier {
			case "mem":
				bs.record(func(r Recorder) { r.MemHit() })
			case "disk":
				bs.record(func(r Recorder) { r.DiskHit() })
			}
			bs.notePrefetchHit(bs.cacheKey(k, ci))
			return d, nil
		}
		bs.record(func(r Recorder) { r.Miss() })
		cs, mine, data, _, cached := bs.claim(k, ci, want)
		if cached {
			return data, nil
		}
		if !mine {
			<-cs.done
			if cs.err != nil {
				return nil, cs.err
			}
			if covers(cs.filled, want) {
				bs.notePrefetchHit(bs.cacheKey(k, ci))
				return cs.data, nil
			}
			continue // the in-flight fill covered fewer extents; re-claim the rest
		}
		if !isPrefetch {
			bs.record(func(r Recorder) { r.UncoveredMiss() })
		}
		merged, newFilled, err := bs.fillExtentSpan(ctx, k, ci, cs.baseData, cs.baseFilled, want, cl, isPrefetch, kind)
		if err != nil {
			bs.complete(k, ci, cs, nil, 0, err, isPrefetch)
			return nil, err
		}
		bs.complete(k, ci, cs, merged, newFilled, nil, isPrefetch)
		return merged, nil
	}
}

// fillExtentSpan GETs exactly the byte span covering the missing extents of chunk
// ci, merges it onto any partial base (copy-on-merge → the merged buffer is a
// fresh immutable ChunkSize slice), verifies the ETag, and returns the merged
// buffer and its new filled bitmap.
func (bs *BlockStore) fillExtentSpan(ctx context.Context, k Key, ci int64, base []byte, baseFilled, want uint16, cl int64, isPrefetch bool, kind fillKind) ([]byte, uint16, error) {
	lo, hi, span, ok := missingByteSpan(baseFilled, want, cl)
	if !ok {
		// base already covers want (raced with another fill); return a copy so the
		// completed buffer is independent.
		return cloneChunk(base, cl), baseFilled, nil
	}

	if isPrefetch {
		select {
		case bs.prefetch <- struct{}{}:
			defer func() { <-bs.prefetch }()
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	select {
	case bs.sem <- struct{}{}:
		defer func() { <-bs.sem }()
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	absOff := ci*ChunkSize + lo
	length := hi - lo
	if got := bs.budget.acquire(length); got > 0 {
		defer bs.budget.release(got)
	}
	bs.record(func(r Recorder) { r.StartInflight() })
	body, etag, err := bs.src.GetRangeReader(ctx, k.Key, absOff, length)
	bs.record(func(r Recorder) { r.S3Get(length, err != nil); r.EndInflight() })
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = body.Close() }()
	if xxh3.HashString(etag) != k.ETagHash {
		bs.markStale(k.Key)
		return nil, 0, ErrStale
	}

	merged := cloneChunk(base, cl)
	// A short read is a truncation, never a zero-padded "success" (H1).
	if _, rerr := io.ReadFull(body, merged[lo:hi]); rerr != nil {
		return nil, 0, rerr
	}
	newFilled := baseFilled | span
	bs.recordFill(kind, length)
	return merged, newFilled, nil
}

// cloneChunk returns a fresh ChunkSize-length (cl) buffer seeded with base's
// bytes (base is nil or already length cl).
func cloneChunk(base []byte, cl int64) []byte {
	buf := make([]byte, cl)
	if len(base) > 0 {
		copy(buf, base)
	}
	return buf
}

// FillRange is the plan-driven byte-exact fetch (#118): it fills exactly the
// extents covering [off, end) — rounded out to 64 KiB — of the object, chunk by
// chunk, at prefetch priority. A format tier-2 plan calls it so a projection
// pulls only its column chunks, not the whole 1 MiB chunks that enclose them.
func (bs *BlockStore) FillRange(ctx context.Context, k Key, off, end, objSize int64) {
	if off < 0 {
		off = 0
	}
	if end > objSize {
		end = objSize
	}
	if end <= off {
		return
	}
	c0 := off / ChunkSize
	c1 := (end - 1) / ChunkSize
	for ci := c0; ci <= c1; ci++ {
		start := ci * ChunkSize
		lo := int64(0)
		if off > start {
			lo = off - start
		}
		hi := end - start
		want := extentMask(lo, hi)
		if want == 0 {
			continue
		}
		if _, err := bs.fetchExtents(ctx, k, ci, want, objSize, true, fillPlan); err != nil {
			return // best-effort, like Prefetch
		}
	}
}

// FillBatch fills the extents covering a batch of byte ranges (#124), coalescing
// them across chunk boundaries into range GETs: each range is extent-aligned
// (64 KiB), then sorted and merged whenever the gap to the next is below
// coalesceGap — bytes inside a closed gap are fetched and marked filled (counted
// as kind=gap so the rounding cost is visible). Each merged run is one coalesced
// GET (via ensureChunks, which fetches a run of owned chunks in one request and
// joins any in-flight chunk). A format tier-2 plan calls this so a projection is
// a few large GETs, not many tiny ones. Best-effort, prefetch priority.
func (bs *BlockStore) FillBatch(ctx context.Context, k Key, ranges []Range, objSize int64) {
	type span struct{ lo, hi int64 }
	aligned := make([]span, 0, len(ranges))
	for _, rg := range ranges {
		lo, hi := rg.Start, rg.End
		if lo < 0 {
			lo = 0
		}
		if hi > objSize {
			hi = objSize
		}
		if hi <= lo {
			continue
		}
		alo := lo / ExtentSize * ExtentSize
		ahi := (hi + ExtentSize - 1) / ExtentSize * ExtentSize
		if ahi > objSize {
			ahi = objSize
		}
		aligned = append(aligned, span{alo, ahi})
	}
	if len(aligned) == 0 {
		return
	}
	sort.Slice(aligned, func(i, j int) bool { return aligned[i].lo < aligned[j].lo })

	// Union of the requested (extent-aligned) ranges — the plan bytes, before any
	// gap-closing. Overlaps are merged so double-counted extents count once.
	var planBytes int64
	{
		u := aligned[0]
		for _, a := range aligned[1:] {
			if a.lo <= u.hi { // overlap/adjacent
				if a.hi > u.hi {
					u.hi = a.hi
				}
				continue
			}
			planBytes += u.hi - u.lo
			u = a
		}
		planBytes += u.hi - u.lo
	}

	// Gap-tolerant coalesce into runs; each run is one GET.
	runs := []span{aligned[0]}
	for _, a := range aligned[1:] {
		last := &runs[len(runs)-1]
		if a.lo <= last.hi+bs.coalesceGap {
			if a.hi > last.hi {
				last.hi = a.hi
			}
			continue
		}
		runs = append(runs, a)
	}

	var mergedBytes int64
	for _, run := range runs {
		mergedBytes += run.hi - run.lo
		bs.fillCoalescedRun(ctx, k, run.lo, run.hi, objSize)
	}
	gapBytes := mergedBytes - planBytes
	if gapBytes < 0 {
		gapBytes = 0
	}
	bs.recordBatch(planBytes, gapBytes, int64(len(runs)))
}

// fillCoalescedRun fetches the extents covering the contiguous byte run [lo, hi)
// in one coalesced GET (ensureChunks coalesces the owned chunks of the run and
// joins any already in flight). Silent kind: FillBatch does the byte accounting.
func (bs *BlockStore) fillCoalescedRun(ctx context.Context, k Key, lo, hi, objSize int64) {
	if hi <= lo {
		return
	}
	c0 := lo / ChunkSize
	c1 := (hi - 1) / ChunkSize
	wantOf := func(ci int64) uint16 {
		start := ci * ChunkSize
		l := int64(0)
		if lo > start {
			l = lo - start
		}
		return extentMask(l, hi-start)
	}
	_ = bs.ensureChunks(ctx, k, c0, c1, objSize, true, nil, wantOf, fillSilent)
}
