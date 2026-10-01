// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// #297: the mount logged the CONFIGURED readahead depth at a point where the effective
// depth was already different, so `--block-size 8MiB --max-readahead 1024` reported 1024
// and delivered 492. The reporting workload only caught it because --pf-trace records
// peak_window. These are the numbers from that measurement.
func TestEffectiveWindowReportsTheBindingBound(t *testing.T) {
	const (
		mib = int64(1) << 20
		// ~12.5% of a 33 GB box: --prefetch-budget is 50% of --mem-cache, itself 25% of RAM.
		budget = int64(4_127_195_136) // 492 * 8 MiB
	)
	// Budget in blocks, as BlockStore.PrefetchBudgetBlocks reports it.
	blocksAt := func(blockSize int64) int64 { return budget / blockSize }
	cases := []struct {
		name         string
		maxRA        int64
		budgetBlocks int64
		wantBlocks   int64
		wantBound    string
	}{
		// The measured case: 1024 configured at 8 MiB, 492 delivered.
		{"budget binds at 8 MiB", 1024, blocksAt(8 * mib), 492, "--prefetch-budget"},
		// Same budget, smaller unit: 3936 blocks available, so the configured depth wins.
		{"configured binds at 1 MiB", 1024, blocksAt(1 * mib), 1024, "--max-readahead"},
		// The shipping default is under the budget bound, so nothing is hidden.
		{"default is not capped", 223, blocksAt(8 * mib), 223, "--max-readahead"},
		// A tiny budget still yields the floor internal/fuse clamps up to.
		{"floor wins", 223, 1, 2, "floor"},
		// Unknown budget must not silently reduce anything.
		{"no budget info", 223, 0, 223, "--max-readahead"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, bound := effectiveWindow(c.maxRA, c.budgetBlocks)
			if got != c.wantBlocks || bound != c.wantBound {
				t.Errorf("effectiveWindow(%d, %d) = (%d, %q), want (%d, %q)",
					c.maxRA, c.budgetBlocks, got, bound, c.wantBlocks, c.wantBound)
			}
		})
	}
}

// The property that made this invisible: which bound binds depends on the block size,
// because the budget is byte-denominated and the window is block-denominated. Halving the
// unit must not reduce the depth the budget permits.
func TestEffectiveWindowBudgetBoundScalesWithBlockSize(t *testing.T) {
	const mib = int64(1) << 20
	budget := int64(4_127_195_136)
	prev := int64(0)
	for _, bs := range []int64{8 * mib, 4 * mib, 2 * mib, 1 * mib} {
		got, _ := effectiveWindow(1<<20, budget/bs) // a depth no budget will reach
		if got <= prev {
			t.Errorf("block size %d MiB: budget permits %d blocks, not more than the %d at the "+
				"larger unit — a byte budget must buy more blocks as the unit shrinks", bs/mib, got, prev)
		}
		prev = got
	}
}
