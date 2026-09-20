// SPDX-License-Identifier: Apache-2.0

package main

// The SHARED-CACHE unit.
//
// Every verdict this tool has produced so far is scored PER HANDLE: a dispatched block
// counts as followed through only if a later read of the SAME handle touches it. On the
// GCHP mounts that unit is wrong, and three gates in a row said so with increasing
// confidence:
//
//   - met is read by ~600 handles over TWELVE keys, with zero sole readers. A block one
//     handle prefetches is routinely read by a different handle.
//   - lith's block cache is per MOUNT, not per handle, so a second dispatch of a chunk
//     that is already resident costs nothing. The per-handle denominator charges a fetch
//     that never happened.
//   - the tool's own cross-check says the same out loud: replay decisions came to 1.00x
//     the mount's own `dispatched` column while the mount FETCHED 3.42x fewer chunks, and
//     per-handle NET cold waste came to 8,403 / 8,513 MiB against a mount cache budget of
//     5.32 GB, in two independent arms. An impossible number is what a double-counted
//     denominator looks like.
//
// So this pass re-scores the same replay against one shared cache per mount:
//
//	DENOMINATOR  a (key, block) pair is charged ONCE, to whichever handle dispatched it
//	             first in decision order. Later dispatches of a resident pair are
//	             counted as suppressed, not as bytes.
//	NUMERATOR    bytes are followed through if ANY handle reads them on that key after
//	             the dispatch that fetched them.
//
// Note what does NOT become global: each handle's detector still sees only its own reads,
// exactly as the live code does. The cache is shared; the state machines are not.
//
// This is only possible because of #271's `seq`. A shared cache has to be replayed in
// mount-wide decision order, and before `seq` no such order was recorded — so rather than
// invent one, this pass refuses to run on a trace without it.
//
// ASSUMPTION, stated because it bounds the answer: no eviction, so shared-cache
// follow-through is an UPPER bound. It is a tight bound on these traces — distinct bytes
// read were 3.03 GiB (met) and 3.44 GiB (HEMCO) against --mem-cache of 24 GB and 32 GB, so
// nothing this pass credits as resident could plausibly have been evicted. On a trace
// where distinct bytes approach the cache size, it would not be.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

// globalStats is the mount-wide view. It reports the per-handle denominator alongside
// the shared one, because the point is the comparison — swapping the unit silently
// would make every banked number incomparable.
type globalStats struct {
	handles          int
	claimedBlocks    int   // (key, block) pairs fetched once
	suppressedBlocks int   // dispatches of a pair already resident
	clampedAway      int   // dispatches falling entirely past EOF
	dispatchedBytes  int64 // deduped
	usedBytes        int64
	perHandleBytes   int64 // the same dispatches charged per handle, for the ratio
	keys             int
	sharedKeys       int // keys read by more than one handle
	crossHandleBytes int64
	sameHandleBytes  int64
	// the cold-start tax in the same unit. This is the number that was impossible:
	// 8,403 MiB of per-handle NET cold waste against a 5.32 GB mount cache.
	coldFirstRunReads int
	coldSuppressed    int // cold reads landing in a chunk another handle already fetched
	coldNetWaste      int64
	coldGrossWaste    int64
	coldNetWastePerHd int64 // the per-handle figure, for the comparison
}

func (g globalStats) followThrough() float64 {
	if g.dispatchedBytes == 0 {
		return math.NaN()
	}
	return float64(g.usedBytes) / float64(g.dispatchedBytes)
}

// globalScore re-scores one trace against a shared cache. The returned handleScores keep
// scoreTrace's features, cold tax and mismatch count untouched — so the #256 rule is fit
// on identical predictors and the fidelity filter still applies — and carry shared-cache
// dispatchedBytes / usedBytes / followThrough in place of the per-handle ones.
func globalScore(perHandle []handleScore, cfg traceConfig, rows []row, byteExact int64) ([]handleScore, globalStats, error) {
	var stats globalStats
	if len(rows) == 0 {
		return nil, stats, fmt.Errorf("no rows")
	}
	for _, r := range rows {
		if r.seq == 0 {
			return nil, stats, fmt.Errorf("trace has no `seq` column: a shared cache must be replayed in mount-wide decision order, and this trace records none (#271)")
		}
		if r.key == "" {
			return nil, stats, fmt.Errorf("trace has no `key` column: dedup is per object, and this trace does not say which object a read is on")
		}
	}

	// Mount-wide decision order.
	ord := make([]row, len(rows))
	copy(ord, rows)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].seq < ord[j].seq })

	type dispatch struct {
		at    int // index into ord
		block int64
		fh    uint64
		key   string
	}
	var dispatches []dispatch
	dets := map[uint64]*prefetch.Prefetcher{}
	byKey := map[string][]int{}  // key -> indices into ord, in decision order
	sizeOf := map[string]int64{} // key -> object size
	highWater := map[string]int64{}
	readersOf := map[string]map[uint64]bool{}

	for i, r := range ord {
		byKey[r.key] = append(byKey[r.key], i)
		if r.size > 0 && sizeOf[r.key] == 0 {
			sizeOf[r.key] = r.size
		}
		if e := r.off + r.length; e > highWater[r.key] {
			highWater[r.key] = e
		}
		if readersOf[r.key] == nil {
			readersOf[r.key] = map[uint64]bool{}
		}
		readersOf[r.key][r.fh] = true
		if r.path != "window" {
			continue
		}
		d, ok := dets[r.fh]
		if !ok {
			d = prefetch.New(cfg.maxReadahead)
			d.SetGapMax(cfg.blockSize)
			d.SetCoverage(cfg.coverageWindow, cfg.coverageMin)
			d.SetEvidence(cfg.evidenceRatio, cfg.blockSize)
			d.Open()
			dets[r.fh] = d
		}
		if r.maxWindow > 0 {
			d.SetMax(r.maxWindow)
		}
		for _, b := range d.Observe(r.blk, r.off, r.length, r.gap) {
			dispatches = append(dispatches, dispatch{at: i, block: b, fh: r.fh, key: r.key})
		}
	}
	// EOF clamp per KEY. Same rule as objSizeOf: the recorded size if the trace has one,
	// else the high-water mark, which under-counts rather than inflating.
	for k, hw := range highWater {
		if sizeOf[k] == 0 {
			sizeOf[k] = hw
		}
	}

	claimed := map[string]map[int64]bool{}
	gDisp := map[uint64]int64{}
	gUsed := map[uint64]int64{}
	suppressed := map[uint64]int{}
	for _, d := range dispatches {
		lo := d.block * cfg.blockSize
		size := sizeOf[d.key]
		if size > 0 && lo >= size {
			stats.clampedAway++
			continue // past EOF: the live fetch returns without doing anything
		}
		hi := lo + cfg.blockSize
		if size > 0 && hi > size {
			hi = size
		}
		stats.perHandleBytes += hi - lo
		if claimed[d.key] == nil {
			claimed[d.key] = map[int64]bool{}
		}
		if claimed[d.key][d.block] {
			stats.suppressedBlocks++
			suppressed[d.fh]++
			continue
		}
		claimed[d.key][d.block] = true
		stats.claimedBlocks++
		stats.dispatchedBytes += hi - lo
		gDisp[d.fh] += hi - lo

		// Redeemed by any later read of this KEY, by any handle.
		var later, same []row
		for _, j := range byKey[d.key] {
			if j <= d.at {
				continue
			}
			later = append(later, ord[j])
			if ord[j].fh == d.fh {
				same = append(same, ord[j])
			}
		}
		used := coveredBytes(later, lo, hi)
		gUsed[d.fh] += used
		stats.usedBytes += used

		// Split the credit by WHO read it. If this is ~0%, the shared unit is just
		// relabelling the per-handle one; if it is large, cross-handle reuse is the
		// thing the per-handle unit was blind to.
		s := coveredBytes(same, lo, hi)
		stats.sameHandleBytes += s
		stats.crossHandleBytes += used - s
	}

	// THE COLD TAX, in the same unit — and this is the one that was impossible. 8,403 MiB
	// of per-handle NET cold waste against a 5.32 GB mount cache cannot be bytes the mount
	// fetched, and the reason is the same as above: coldTax dedupes chunks WITHIN a handle
	// (`seen`), so when 121 handles each cold-read the same chunk of the same object, it
	// charges 121 whole-chunk fetches for one. Here a (key, chunk) is charged once
	// mount-wide, and "never read" means never read by ANY handle on that key.
	allOf := map[string][]row{}
	for k, idxs := range byKey {
		rs := make([]row, 0, len(idxs))
		for _, j := range idxs {
			rs = append(rs, ord[j])
		}
		allOf[k] = rs
	}
	coldSeen := map[string]map[int64]bool{}
	notFirstRun := map[uint64]bool{}
	cFirst := map[uint64]int{}
	cNet := map[uint64]int64{}
	cGross := map[uint64]int64{}
	for _, r := range ord {
		if r.before != "cold" {
			notFirstRun[r.fh] = true // the handle has been classified at least once
			continue
		}
		if r.path != "window" || r.length > byteExact || r.length >= chunkSize {
			continue
		}
		if notFirstRun[r.fh] {
			continue // a re-entry into cold: not the pre-decision phase, same as coldTax
		}
		cFirst[r.fh]++
		stats.coldFirstRunReads++
		ci := r.off / chunkSize
		if coldSeen[r.key] == nil {
			coldSeen[r.key] = map[int64]bool{}
		}
		if coldSeen[r.key][ci] {
			stats.coldSuppressed++
			continue // already resident: a cache hit, and it costs nothing
		}
		coldSeen[r.key][ci] = true
		lo := ci * chunkSize
		hi := lo + chunkSize
		if size := sizeOf[r.key]; size > 0 && hi > size {
			hi = size
		}
		if hi <= lo {
			continue
		}
		g := (hi - lo) - r.length
		n := (hi - lo) - coveredBytes(allOf[r.key], lo, hi)
		cGross[r.fh] += g
		cNet[r.fh] += n
		stats.coldGrossWaste += g
		stats.coldNetWaste += n
	}

	stats.keys = len(byKey)
	for _, rs := range readersOf {
		if len(rs) > 1 {
			stats.sharedKeys++
		}
	}

	out := make([]handleScore, 0, len(perHandle))
	for _, s := range perHandle {
		stats.coldNetWastePerHd += s.coldNetWasteBytes
		s.coldFirstRunReads = cFirst[s.fh]
		s.coldNetWasteBytes = cNet[s.fh]
		s.coldGrossWasteByte = cGross[s.fh]
		s.dispatchedBytes = gDisp[s.fh]
		s.usedBytes = gUsed[s.fh]
		s.suppressedBlocks = suppressed[s.fh]
		if s.dispatchedBytes > 0 {
			s.followThrough = float64(s.usedBytes) / float64(s.dispatchedBytes)
		} else {
			// Every block this handle asked for was already resident, or it asked for
			// none. Either way it caused no fetch, so it has no follow-through to score
			// — NaN, which the verdict already excludes, rather than a 0 that would
			// drag the distribution down with handles that wasted nothing.
			s.followThrough = math.NaN()
		}
		out = append(out, s)
	}
	stats.handles = len(out)
	return out, stats, nil
}

// parseIssuedPer reads `met/a=2939,hemco/a=4632`: the mount's own
// lith_prefetch_issued_total for each trace. The existing -issued flag takes one total
// for the whole run, which is right for the per-handle denominator but useless here —
// whether the shared unit reproduces the mount's fetch volume is a per-MOUNT question,
// and met and HEMCO answer it differently (4.5x vs 1.5x per-handle inflation).
func parseIssuedPer(spec string) map[string]int64 {
	out := map[string]int64{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(k)] = n
	}
	return out
}

func reportGlobal(g globalStats, perHandleDispBytes, perHandleUsedBytes, issued, blockSize int64) {
	fmt.Printf("   SHARED-CACHE unit (one cache per mount, no eviction => an upper bound):\n")
	fmt.Printf("     blocks fetched once %d, suppressed as already resident %d  => %.2fx dedup\n",
		g.claimedBlocks, g.suppressedBlocks, ratioOf(g.perHandleBytes, g.dispatchedBytes))
	fmt.Printf("     dispatched %.1f MiB shared vs %.1f MiB per-handle;  followed through %.1f MiB vs %.1f MiB\n",
		mibOf(g.dispatchedBytes), mibOf(perHandleDispBytes), mibOf(g.usedBytes), mibOf(perHandleUsedBytes))
	fmt.Printf("     byte follow-through %.3f shared vs %.3f per-handle\n",
		g.followThrough(), ratioOf(perHandleUsedBytes, perHandleDispBytes))
	fmt.Printf("     of the redeemed bytes, %.1f%% were read by a DIFFERENT handle than the one that fetched them\n",
		100*ratioOf(g.crossHandleBytes, g.usedBytes))
	fmt.Printf("     keys %d, read by more than one handle %d;  dispatches clamped past EOF %d\n",
		g.keys, g.sharedKeys, g.clampedAway)
	fmt.Printf("     cold tax, shared: %d first-run small reads, %d suppressed as already resident\n",
		g.coldFirstRunReads, g.coldSuppressed)
	fmt.Printf("       NET waste %.1f MiB shared vs %.1f MiB per-handle  (%.2fx)   [gross %.1f MiB]\n",
		mibOf(g.coldNetWaste), mibOf(g.coldNetWastePerHd),
		ratioOf(g.coldNetWastePerHd, g.coldNetWaste), mibOf(g.coldGrossWaste))
	// The independent check, and the only one that can adjudicate between the two units:
	// the mount counted the chunks it actually prefetched, and neither replay can argue
	// with it. Reported per mount because that is the granularity the counter has.
	if issued > 0 && blockSize > 0 {
		shared := g.dispatchedBytes / chunkSize
		perHd := g.perHandleBytes / chunkSize
		fmt.Printf("     vs the MOUNT's own lith_prefetch_issued_total = %d chunks:\n", issued)
		fmt.Printf("       shared %d chunks -> %.2fx    per-handle %d chunks -> %.2fx\n",
			shared, ratioOf(shared, issued), perHd, ratioOf(perHd, issued))
	}
}

func mibOf(b int64) float64 { return float64(b) / (1 << 20) }

func ratioOf(a, b int64) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}
