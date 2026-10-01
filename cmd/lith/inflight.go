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
// bandwidth-delay product), the mount-wide prefetch budget (a fraction of RAM, now the hard
// admission cap), and the in-flight cap (NIC baseline x latency, a blocking semaphore in the
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
