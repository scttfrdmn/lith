// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

// DRIVING THE REAL MEMORY TIER.
//
// Every #256 gate so far has reported its shared-cache numbers with the same caveat: no
// eviction, therefore an upper bound on redemption, "tight on these traces (3.0-3.4 GiB
// distinct against 24-32 GB of cache) and unsafe in general." Three gates have now
// inherited that sentence. It is an assertion, and this replaces it with a measurement.
//
// The model is not a model of the policy — it drives `blockstore.CacheModel`, which drives
// the production `memCache`, sharded as production shards it. Writing a 2Q simulator from
// the paper would have been wrong twice over: the tier does not enforce the classic Kin
// bound (#279), and a freshly filled prefetch chunk is not yet flagged unread when
// eviction runs (#280), so a from-the-paper model would have been more scan-resistant AND
// more protective of prefetch than lith actually is. Both were found by reading the
// eviction path in order to drive it.
//
// What this cannot reproduce, stated rather than discovered later:
//
//   - The chunk key. Production keys a chunk as `bucket|key|etaghash|chunkIdx`; the trace
//     records none of bucket or ETag. Shard assignment is a hash of that string, so the
//     model's key format gives a statistically equivalent but not identical distribution
//     across the 64 shards. Aggregate eviction pressure is preserved; which two specific
//     chunks collide in a shard is not.
//   - Pin lifetimes. A chunk is unevictable while a reader holds it. The model never pins,
//     which makes it slightly MORE willing to evict than production — so it errs toward
//     reporting eviction as binding, which is the safe direction for a caveat whose whole
//     purpose is to bound optimism.
//   - Time. The replay walks decision order, not wall clock, so a prefetch that in
//     production landed after the read that would have redeemed it is credited here as
//     arriving before. That is the same assumption the rest of the shared-cache accounting
//     makes, and it is why this is still an upper bound on redemption -- just a much
//     tighter one, now bounded by capacity as well as by order.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// driveCache walks the replay in decision order, filling and reading the modelled tier as
// the mount would have, and returns what eviction cost.
//
// It is handed the very slices globalScore built — `ord` and `dispatches` — so the detector
// replay that produced them is not repeated. At each step the demand read is served first
// and the prefetch dispatches it triggered are filled second, matching internal/fuse's
// order (the read is served, then `go f.store.Prefetch` is launched).
func driveCache(m *blockstore.CacheModel, ord []row, dispatches []dispatch, sizeOf map[string]int64, blockSize int64) {
	// Dispatches bucketed by the decision they belong to, so one pass suffices.
	byAt := map[int][]dispatch{}
	for _, d := range dispatches {
		byAt[d.at] = append(byAt[d.at], d)
	}

	clamp := func(key string, lo, hi int64) (int64, int64) {
		if size := sizeOf[key]; size > 0 && hi > size {
			hi = size
		}
		return lo, hi
	}

	for i, r := range ord {
		// The demand read. Every chunk it covers is referenced; a miss is a demand fill,
		// which is how the tier is populated outside prefetch.
		if r.length > 0 {
			first, last := r.off/chunkSize, (r.off+r.length-1)/chunkSize
			for ci := first; ci <= last; ci++ {
				lo, hi := clamp(r.key, ci*chunkSize, ci*chunkSize+chunkSize)
				if hi <= lo {
					continue
				}
				ck := modelChunkKey(r.key, ci)
				if !m.Read(ck) {
					m.Fill(ck, hi-lo, false)
				}
			}
		}
		// The prefetch fills that decision triggered. A block is blockSize/chunkSize
		// chunks; the live Prefetch is EOF-clamped, so a block past the end fills nothing.
		for _, d := range byAt[i] {
			blo := d.block * blockSize
			for off := blo; off < blo+blockSize; off += chunkSize {
				ci := off / chunkSize
				lo, hi := clamp(d.key, off, off+chunkSize)
				if hi <= lo {
					continue
				}
				m.Fill(modelChunkKey(d.key, ci), hi-lo, true)
			}
		}
	}
}

// modelChunkKey mirrors the SHAPE of blockstore's cacheKey (a delimited key plus chunk
// index) without the bucket and ETag the trace does not record. See the file comment: this
// changes which chunks share a shard, not how many chunks compete for one.
func modelChunkKey(key string, ci int64) string {
	return key + "|" + strconv.FormatInt(ci, 10)
}

// reportEviction prints what the no-eviction assumption was hiding. On a trace where the
// working set fits, the interesting output is the first line: the caveat three gates
// carried is now checked rather than asserted.
func reportEviction(label string, capacity int64, s blockstore.CacheModelStats, binding bool) {
	fmt.Printf("   -- EVICTION model (%s): the real memory tier, %s across 64 shards (%s/shard)\n",
		label, human(capacity), human(capacity/64))
	if !binding {
		fmt.Printf("      NOT BINDING on this trace: %d fills, 0 re-fetches, 0 unread evictions.\n", s.Fills)
		fmt.Printf("      So the no-eviction upper bound the shared-cache numbers carry is EXACT here,\n")
		fmt.Printf("      measured rather than assumed. It says nothing about a larger working set.\n")
		return
	}
	fmt.Printf("      BINDING: %d fills, of which %d were RE-fetches of an evicted chunk (%.1f MiB)\n",
		s.Fills, s.Refills, mib(s.RefillBytes))
	fmt.Printf("      %d prefetched chunks evicted before any read consumed them (%.1f MiB unredeemable)\n",
		s.UnreadEvicts, mib(s.UnreadBytes))
	if s.Reads > 0 {
		fmt.Printf("      demand residency %d/%d (%.1f%%)\n",
			s.Hits, s.Reads, 100*float64(s.Hits)/float64(s.Reads))
	}
	fmt.Printf("      Every shared-cache figure above is therefore optimistic by at least the\n")
	fmt.Printf("      re-fetched bytes, and follow-through by the unredeemable ones.\n")
}

// memShards is production's memory-tier shard count (internal/blockstore/blockstore.go).
// Hard-coded rather than flagged because it is policy, not configuration: a replay that
// pooled the capacity would over-report residency.
const memShards = 64

// parseBytes reads a capacity like "24GB", "512MiB", "1073741824". Decimal and binary
// suffixes both, because `--mem-cache` is quoted both ways in this project's own
// measurements and silently reading one as the other is exactly the MiB/MB error that cost
// a gate.
func parseBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	} {
		if strings.HasSuffix(s, suf.s) {
			mult, s = suf.m, strings.TrimSuffix(s, suf.s)
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "-mem-cache: cannot parse %q as a capacity; eviction not modelled\n", s)
		return 0
	}
	return n * mult
}
