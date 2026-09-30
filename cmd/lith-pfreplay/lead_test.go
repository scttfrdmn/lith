// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

import "testing"

// The lead is the whole point of this pass, so its arithmetic is asserted directly rather
// than inferred from a summary: a block dispatched at decision i and demanded at decision
// j must report a lead of j-i, and a read that arrives at or before the dispatch must be
// counted as self-fetching rather than given a nonsensical negative lead.
func TestLeadCountsReadsBetweenDispatchAndDemand(t *testing.T) {
	const blk = int64(8 << 20)
	ord := make([]row, 10)
	for i := range ord {
		ord[i] = row{seq: int64(i + 1), fh: 1, key: "k", size: 100 * blk,
			off: int64(i) * blk, length: 1 << 20, path: "window"}
	}
	// Block 5 dispatched at decision 2; demanded at decision 5 -> lead 3.
	// Block 9 dispatched at decision 9, i.e. the same decision that demands it.
	ds := []dispatch{{at: 2, block: 5, fh: 1, key: "k"}, {at: 9, block: 9, fh: 1, key: "k"}}
	got := scoreLead(ord, ds, blk)

	if got.reads != 1 {
		t.Fatalf("dispatched-earlier reads = %d, want 1", got.reads)
	}
	if got.leads[0] != 3 {
		t.Errorf("lead = %d, want 3 (dispatched at 2, demanded at 5)", got.leads[0])
	}
	if got.selfFirst != 1 {
		t.Errorf("selfFirst = %d, want 1 (block 9 dispatched at the decision that demands it)", got.selfFirst)
	}
	if got.neverDisp != 8 {
		t.Errorf("neverDisp = %d, want 8", got.neverDisp)
	}
}

// A block dispatched more than once must be credited to its FIRST dispatch: the store's
// singleflight suppresses the later ones, so the first is the fetch a later read would
// join. Crediting the last would understate the lead and manufacture the very short leads
// this pass exists to look for.
func TestLeadUsesTheFirstDispatchOfABlock(t *testing.T) {
	const blk = int64(8 << 20)
	ord := make([]row, 8)
	for i := range ord {
		ord[i] = row{seq: int64(i + 1), fh: 1, key: "k", size: 100 * blk,
			off: int64(i) * blk, length: 1 << 20, path: "window"}
	}
	ds := []dispatch{
		{at: 5, block: 7, fh: 1, key: "k"}, // later dispatch listed first
		{at: 1, block: 7, fh: 1, key: "k"}, // the real one
	}
	got := scoreLead(ord, ds, blk)
	if len(got.leads) != 1 {
		t.Fatalf("want one scored read, got %d", len(got.leads))
	}
	if got.leads[0] != 6 {
		t.Errorf("lead = %d, want 6 (first dispatch at 1, demanded at 7)", got.leads[0])
	}
}
