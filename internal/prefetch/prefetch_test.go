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
