// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// #301: the floor condition must be visible at mount, not only in a trace.
//
// The workload that found #301 ran both production mounts at the 2-block floor and could
// not tell: the mount logged the configured depth, and the descriptors doing the
// throttling belonged to other processes. These two numbers are what it needed.
func TestWindowCoverage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		effW, budget  int64
		full, floorAt int64
	}{
		// The shipping default on a 33 GB box: 4.128 GB budget at 8 MiB blocks is 492
		// blocks, window 223. Fifteen descriptors hold a full window; past 247 everyone
		// is at the floor. The reporting workload's ~288 is past it.
		{"shipping default", 223, 492, 2, 165},
		// Their measured mount, 1 MiB blocks: the budget is blocks-plentiful, so the
		// floor is far away and the full-window count is large.
		{"1 MiB blocks", 223, 3936, 17, 1313},
		// A budget that cannot cover even one declared window. This is #298's silent
		// override -- effectiveWindow would already have reduced effW, so seeing full=0
		// here means the caller passed the CONFIGURED depth, which the warning catches.
		{"budget below one window", 1024, 492, 0, 165},
		// Exactly one: the mount is fine alone and halves on the second descriptor.
		{"one descriptor's worth", 492, 492, 1, 165},
		// Degenerate budgets. A one-block budget is at the floor from the first
		// descriptor, so floorAt is 1 rather than 0.
		{"single block budget", 223, 1, 0, 1},
		{"no budget", 223, 0, 0, 0},
		{"no window", 0, 492, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			full, floorAt := windowCoverage(tc.effW, tc.budget)
			if full != tc.full || floorAt != tc.floorAt {
				t.Errorf("windowCoverage(effW=%d, budget=%d) = (%d, %d), want (%d, %d)",
					tc.effW, tc.budget, full, floorAt, tc.full, tc.floorAt)
			}
		})
	}
}

// The floor count must agree with the divisor itself rather than restating its arithmetic.
// internal/fuse owns perHandleWindow; this reproduces its clamp locally and checks that
// floorAt is the FIRST count at which the share is 2, so the two cannot drift silently.
func TestWindowCoverageFloorAgreesWithTheDivisor(t *testing.T) {
	share := func(budgetBlocks, n, maxW int64) int64 {
		s := budgetBlocks / n
		if s < 2 {
			s = 2
		}
		if s > maxW {
			s = maxW
		}
		return s
	}
	for _, budget := range []int64{7, 8, 9, 100, 492, 3936} {
		const maxW = 223
		_, floorAt := windowCoverage(maxW, budget)
		if floorAt == 0 {
			t.Fatalf("budget=%d: no floor reported", budget)
		}
		if got := share(budget, floorAt, maxW); got != 2 {
			t.Errorf("budget=%d: at the reported floor count %d the share is %d, want 2",
				budget, floorAt, got)
		}
		if floorAt > 1 {
			if got := share(budget, floorAt-1, maxW); got <= 2 {
				t.Errorf("budget=%d: one descriptor below the reported floor (%d) the share "+
					"is already %d, so the floor is reached earlier than reported",
					budget, floorAt-1, got)
			}
		}
	}
}
