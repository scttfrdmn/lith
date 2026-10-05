// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

// defaultInflightFallback is used when the NIC bandwidth cannot be determined.
const defaultInflightFallback = 512 << 20

// computeInflightBytes resolves the bytes-in-flight budget. An explicit
// --inflight-bytes flag wins; otherwise it is 2 × (baselineGbps × 100 ms) — the
// bandwidth-delay product sized from the sustained **baseline** so a long read
// does not over-commit once burst credits deplete (#79) — or a 512 MiB fallback
// when the NIC speed is unknown (baselineGbps <= 0). Returns the budget and a
// human description.
func computeInflightBytes(flag string, baselineGbps float64) (int64, string) {
	if flag != "" {
		n, err := parseSize(flag)
		if err == nil && n > 0 {
			return n, fmt.Sprintf("%d bytes (from --inflight-bytes)", n)
		}
	}
	if baselineGbps > 0 {
		// 2 × bandwidth-delay product at 100 ms, from the baseline.
		n := int64(2 * baselineGbps * 1e9 / 8 * 0.1)
		return n, fmt.Sprintf("%d bytes (2 × %.1f Gbps baseline × 100 ms)", n, baselineGbps)
	}
	return defaultInflightFallback, fmt.Sprintf("%d bytes (fallback; NIC speed unknown)", int64(defaultInflightFallback))
}

// effectiveReadahead resolves the per-handle readahead window in blocks. A
// positive user value wins; otherwise the default is the bandwidth-delay
// product (inflightBytes / block) so a single reader can hold enough in flight
// to fill a fat NIC on a cold read (#56), clamped to [8, 1024] blocks.
func effectiveReadahead(userBlocks, inflightBytes, blockSize int64) int64 {
	if userBlocks > 0 {
		return userBlocks
	}
	if blockSize <= 0 {
		blockSize = 8 << 20
	}
	// 1.5 × the BDP in blocks: the window must lead by more than inflight-bytes
	// because completed chunks sit cached-unread ahead of the cursor, so the
	// bytes actually in flight are less than the window. Measured knee on a
	// 30 Gbps box: a raw BDP window (~89 blocks) reached only 88% of mount-s3,
	// 1.5× (~134) reaches parity+ (#56).
	n := inflightBytes * 3 / (2 * blockSize)
	if n < 8 {
		n = 8
	}
	if n > 1024 {
		n = 1024
	}
	return n
}

// effectiveWindow reports the readahead depth a SINGLE handle will actually be given,
// and which bound produced it (#297).
//
// Two bounds apply and the smaller wins: the configured-or-derived --max-readahead, and
// the prefetch budget in blocks (internal/fuse perHandleWindow divides that by the live
// handle count, so one handle gets all of it). The budget is BYTE-denominated, so which
// bound binds changes with --block-size: on a 33 GB box the budget is ~4.13 GB, which is
// 492 blocks at 8 MiB and 3936 at 1 MiB. The budget-in-blocks figure comes from
// BlockStore.PrefetchBudgetBlocks rather than being re-divided here, so the two cannot
// drift.
//
// This exists because the mount used to log the CONFIGURED depth at a point where the
// effective depth was already different: --block-size 8MiB --max-readahead 1024 logged
// 1024 and delivered 492, silently, so a readahead measurement at a non-default block
// size was not the experiment it was configured to be. See #298 for the broader problem
// that the three bounds disagree by 1.5x in the shipping default.
func effectiveWindow(maxReadahead, budgetBlocks int64) (int64, string) {
	eff, bound := maxReadahead, "--max-readahead"
	if budgetBlocks > 0 && budgetBlocks < eff {
		eff, bound = budgetBlocks, "--prefetch-budget"
	}
	// internal/fuse clamps a handle's share up to 2 blocks however tight the budget.
	if eff < 2 {
		eff, bound = 2, "floor"
	}
	return eff, bound
}

// bindingBound names which of the three bounds on outstanding prefetch is smallest, and so
// which one a tuner is actually up against (#298).
//
// The three measure different things and are derived from unrelated quantities: one handle's
// window commitment (--max-readahead x --block-size, an empirical 1.5x multiple of the
// bandwidth-delay product), the mount-wide prefetch budget (a fraction of RAM, divided across
// handles), and the in-flight cap (NIC baseline x latency, a blocking semaphore in the
// blockstore). In the shipping default they disagree by 1.5x and the smallest wins silently,
// which is why raising --max-readahead from 223 to 492 once measured +3%: both configurations
// were already against a ceiling neither of them set.
//
// Each is legitimate on its own terms -- what the link can hold, what RAM can hold un-demanded,
// what the NIC wants fed -- so this names the binding one rather than forcing them to agree.
func bindingBound(windowCommit, prefetchBudget, inflightBytes int64) string {
	smallest, name := windowCommit, "--max-readahead x --block-size"
	if prefetchBudget > 0 && prefetchBudget < smallest {
		smallest, name = prefetchBudget, "--prefetch-budget"
	}
	if inflightBytes > 0 && inflightBytes < smallest {
		name = "--inflight-bytes"
	}
	return name
}

// windowCoverage reports how many concurrent open descriptors the prefetch budget can
// serve, at two depths: `full` get the whole effective window, and at `floorAt` every
// reader is down to the 2-block floor (#301).
//
// Both follow from the divisor in internal/fuse perHandleWindow -- each handle gets
// clamp(budgetBlocks/N, 2, maxReadahead) -- so full = budgetBlocks/effW and the floor
// binds once budgetBlocks/N < 2.
//
// This is reported at mount because the condition is otherwise invisible. The workload
// that found #301 ran BOTH production mounts at the floor of 2 blocks -- 48 ranks x ~6
// files is ~288 descriptors against the ~246 needed to get there -- reading at 186 MB/s
// where 1293 was available, with no flag set wrong and nothing in the log saying so. The
// mount logged the configured depth; the descriptors doing the throttling belonged to
// other processes. Two numbers at startup would have shown it.
//
// floorAt is 1 when the mount is at the floor from its very first descriptor.
//
// The floor is where the share is 2, which integer division reaches one step EARLIER than
// where the clamp starts applying: at budgetBlocks=492 the share at N=246 is already
// exactly 2 without any clamping, so the condition is budgetBlocks/N < 3, not < 2. A
// cross-check against the divisor's own arithmetic caught that off-by-one; the test
// reproduces the clamp rather than restating this formula, so the two cannot agree by
// construction.
func windowCoverage(effW, budgetBlocks int64) (full, floorAt int64) {
	if budgetBlocks <= 0 || effW <= 0 {
		return 0, 0
	}
	full = budgetBlocks / effW
	if budgetBlocks < 3 || effW <= 2 {
		// One descriptor is already at the floor -- either the budget cannot fund more
		// than the floor, or the effective window IS the floor.
		return full, 1
	}
	return full, budgetBlocks/3 + 1
}

// pressureMaxWarning returns a warning for a --prefetch-pressure-max that cannot do what it
// is being asked to do, and "" for a usable one (#313).
//
// Above 1.0 the flag is useless BY ARITHMETIC, not by measurement: it admits more unread
// bytes than the tier can hold, so the tier fills and must evict one to take another --
// which is the exact condition the gate exists to prevent. Measured at 1.3 on real S3: the
// gate fired 94-100 times, held peak pressure at 1.300 as designed, and still evicted
// 2051-2235 unread chunks.
//
// A warning rather than a clamp: an operator who typed a number gets to keep it, and
// silently substituting a different one is how a knob stops meaning anything. The measured
// band is 0.85-1.0.
func pressureMaxWarning(v float64) string {
	switch {
	case v <= 0:
		return ""
	case v > 1.0:
		return fmt.Sprintf("--prefetch-pressure-max %g admits %.0f%% more unread bytes than the memory tier can hold, so the tier still fills and still evicts unread chunks — measured at 1.3, the gate fired and bounded pressure exactly as asked and evictions only fell from ~2900 to ~2100. Any value above 1.0 cannot work; the measured band is 0.85-1.0 (#313)", v, (v-1)*100)
	case v < 0.5:
		return fmt.Sprintf("--prefetch-pressure-max %g is below the lowest value measured. At 0.5 the gate over-throttled by 55%% against 1.0 (63 s vs 41 s) while buying nothing over 0.85, so lower values are likely to cost wall clock for no further reduction in evictions (#313)", v)
	}
	return ""
}
