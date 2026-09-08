// SPDX-License-Identifier: Apache-2.0

package prefetch

import (
	"reflect"
	"testing"
)

func TestOpenDispatchesInitialWindow(t *testing.T) {
	p := New(32)
	if got := p.Open(); !reflect.DeepEqual(got, []int64{0, 1}) {
		t.Fatalf("Open() = %v, want [0 1]", got)
	}
}

// TestSequentialFrontierLeadsNoDup: over a sequential run, every block is
// dispatched exactly once and the frontier stays at least `window` ahead of the
// demand cursor.
func TestSequentialFrontierLeadsNoDup(t *testing.T) {
	p := New(32)
	dispatched := append([]int64{}, p.Open()...) // [0,1]

	for blk := int64(0); blk <= 40; blk++ {
		got := p.Observe(blk)
		// The frontier must lead the cursor by at least the current window.
		if p.frontier-blk < p.window {
			t.Fatalf("after read %d: frontier %d - cursor %d = %d < window %d",
				blk, p.frontier, blk, p.frontier-blk, p.window)
		}
		dispatched = append(dispatched, got...)
	}
	if p.State() != Sequential {
		t.Fatalf("state = %v, want Sequential", p.State())
	}
	// Dispatched blocks must be exactly 0..frontier-1, contiguous, no dups.
	for i, b := range dispatched {
		if b != int64(i) {
			t.Fatalf("dispatch not contiguous/unique at position %d: got block %d", i, b)
		}
	}
	if int64(len(dispatched)) != p.frontier {
		t.Fatalf("dispatched %d blocks, frontier %d", len(dispatched), p.frontier)
	}
}

func TestSequentialWindowGrowsGeometrically(t *testing.T) {
	p := New(32)
	p.Open()
	var windows []int64
	for blk := int64(0); blk <= 6; blk++ {
		p.Observe(blk)
		windows = append(windows, p.window)
	}
	// window: 2,4,8,16,32,32,32 (capped at maxReadahead=32)
	want := []int64{2, 4, 8, 16, 32, 32, 32}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows = %v, want %v", windows, want)
	}
}

func TestStridedPredictsNext(t *testing.T) {
	p := New(32)
	// delta 5: confirmed on the second delta, then predicts one block ahead.
	got := [][]int64{
		p.Observe(0),  // cold: establish last
		p.Observe(5),  // first delta 5, not confirmed
		p.Observe(10), // confirmed -> predict 15
		p.Observe(15), // predict 20
	}
	want := [][]int64{nil, nil, {15}, {20}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("strided dispatches = %v, want %v", got, want)
	}
	if p.State() != Strided {
		t.Errorf("state = %v, want Strided", p.State())
	}
}

func TestRandomDispatchesNothing(t *testing.T) {
	p := New(32)
	seq := []int64{0, 7, 2, 9, 1}
	for i, b := range seq {
		if got := p.Observe(b); len(got) != 0 && i > 0 {
			// After the first (which just records), random reads dispatch nothing.
			if p.State() == Random {
				t.Errorf("random read %d dispatched %v", b, got)
			}
		}
	}
	if p.State() != Random {
		t.Errorf("state = %v, want Random", p.State())
	}
}

func TestReReadSameBlockNoop(t *testing.T) {
	p := New(32)
	p.Open()
	p.Observe(0)
	if got := p.Observe(0); got != nil {
		t.Errorf("re-read same block dispatched %v, want nil", got)
	}
}

// sumDispatched runs a scripted read sequence (after Open) and returns the
// total blocks dispatched and the set of dispatched blocks.
func sumDispatched(p *Prefetcher, reads []int64) (int, map[int64]bool) {
	set := map[int64]bool{}
	add := func(bs []int64) {
		for _, b := range bs {
			set[b] = true
		}
	}
	add(p.Open())
	for _, b := range reads {
		add(p.Observe(b))
	}
	return len(set), set
}

// TestInBandReorderNoHalving: out-of-order-but-sequential reads never halve the
// window, dispatch the same blocks as a strictly sequential read (±1), and
// every read is covered by a prior dispatch (uncovered == 0).
func TestInBandReorderNoHalving(t *testing.T) {
	reorder := []int64{0, 1, 3, 2, 5, 4, 6, 7}
	sorted := []int64{0, 1, 2, 3, 4, 5, 6, 7}

	pr := New(32)
	// Coverage check: each read must already be dispatched when it arrives.
	covered := map[int64]bool{}
	for _, b := range pr.Open() {
		covered[b] = true
	}
	for _, b := range reorder {
		if !covered[b] {
			t.Fatalf("read block %d was uncovered (prefetch did not lead)", b)
		}
		for _, d := range pr.Observe(b) {
			covered[d] = true
		}
	}
	if pr.Halvings() != 0 {
		t.Fatalf("in-band reorder halved the window %d times, want 0", pr.Halvings())
	}
	if pr.Resets() != 0 {
		t.Fatalf("in-band reorder reset to random %d times, want 0", pr.Resets())
	}
	if pr.State() != Sequential {
		t.Fatalf("state = %v, want Sequential", pr.State())
	}

	nReorder, _ := sumDispatched(New(32), reorder)
	nSorted, _ := sumDispatched(New(32), sorted)
	if diff := nReorder - nSorted; diff < -1 || diff > 1 {
		t.Fatalf("reorder dispatched %d blocks, sequential %d (diff %d, want ±1)", nReorder, nSorted, diff)
	}
}

// TestSingleSeekHalvesOnce: a sequential run then one far jump halves the window
// exactly once (never below 2), resumes dispatch at the seek target without
// re-ramping from zero, and grows again on the following reads.
func TestSingleSeekHalvesOnce(t *testing.T) {
	p := New(32)
	p.Open()
	for b := int64(0); b <= 20; b++ {
		p.Observe(b)
	}
	winBefore := p.window
	got := p.Observe(200)
	if p.Halvings() != 1 {
		t.Fatalf("halvings = %d, want 1", p.Halvings())
	}
	if p.window < initialWindow {
		t.Fatalf("window = %d after seek, want >= %d", p.window, initialWindow)
	}
	if p.window != winBefore/2 {
		t.Fatalf("window = %d after seek, want %d (halved)", p.window, winBefore/2)
	}
	seen := map[int64]bool{}
	for _, b := range got {
		seen[b] = true
	}
	if !seen[200] {
		t.Fatalf("seek did not dispatch at the target: got %v", got)
	}
	if seen[0] || p.frontier < 200 {
		t.Fatalf("seek re-ramped from 0: frontier=%d dispatch=%v", p.frontier, got)
	}
	winAfterSeek := p.window
	p.Observe(201)
	p.Observe(202)
	if p.window <= winAfterSeek {
		t.Fatalf("window did not grow after seek: %d -> %d", winAfterSeek, p.window)
	}
	if p.Resets() != 0 {
		t.Fatalf("single seek reset to random %d times, want 0", p.Resets())
	}
}

// TestDoubleSeekRandomThenRecover: two seeks with no sequential progress between
// them fall to Random; a subsequent sequential run re-enters Sequential.
func TestDoubleSeekRandomThenRecover(t *testing.T) {
	p := New(32)
	p.Open()
	for b := int64(0); b <= 20; b++ {
		p.Observe(b)
	}
	p.Observe(200) // first seek
	p.Observe(400) // second seek, no progress between
	if p.State() != Random {
		t.Fatalf("state after double seek = %v, want Random", p.State())
	}
	if p.Resets() != 1 {
		t.Fatalf("resets = %d, want 1", p.Resets())
	}
	p.Observe(401)
	p.Observe(402)
	p.Observe(403)
	if p.State() != Sequential {
		t.Fatalf("state after recovery = %v, want Sequential", p.State())
	}
}

// TestStrideUnchanged: a constant non-unit delta is detected as strided and
// dispatches only the single next predicted block (evaluated before the seek
// rule).
func TestStrideUnchanged(t *testing.T) {
	p := New(32)
	got := [][]int64{
		p.Observe(0),  // establish
		p.Observe(8),  // first delta 8, unconfirmed
		p.Observe(16), // confirmed -> predict 24
		p.Observe(24), // predict 32
	}
	want := [][]int64{nil, nil, {24}, {32}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("strided dispatches = %v, want %v", got, want)
	}
	if p.State() != Strided {
		t.Fatalf("state = %v, want Strided", p.State())
	}
	if p.Halvings() != 0 {
		t.Fatalf("strided halved the window %d times, want 0", p.Halvings())
	}
}
