// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

const bs = int64(8 << 20) // an 8 MiB block, the fuse-layer seqGapMax

// A scattered ascending walk — same block sequence as a stream (0,1,2…), but
// each read is kilobytes separated by a multi-block byte gap — must classify
// Random and stop prefetching (#210/M16 1b).
func TestGapWalkClassifiesRandom(t *testing.T) {
	p := New(16)
	p.SetGapMax(bs)
	p.Open()
	p.Observe(0, 0)           // first read, contiguous — establishes Sequential
	p.Observe(1, 3*bs)        // adjacent block but a 24 MiB gap → a seek, not progress
	got := p.Observe(2, 3*bs) // second seek with no progress between → Random
	if p.State() != Random {
		t.Fatalf("scattered walk: state = %v, want Random", p.State())
	}
	if len(got) != 0 {
		t.Fatalf("random walk dispatched %v prefetch blocks; want none", got)
	}
	if got := p.Observe(3, 3*bs); len(got) != 0 {
		t.Fatalf("random walk still dispatching %v; want none", got)
	}
}

// The mirror-image guard, the test that matters most: a contiguous ascending
// scan — the same block sequence, but gap 0 (each block advance is contiguous) —
// must stay Sequential and prefetch. If this ever fails, the rule is
// declassifying real streams and must not merge.
func TestGapContiguousScanStaysSequential(t *testing.T) {
	p := New(16)
	p.SetGapMax(bs)
	p.Open()
	p.Observe(0, 0)
	got := p.Observe(1, 0) // contiguous advance
	if p.State() != Sequential {
		t.Fatalf("contiguous scan: state = %v, want Sequential", p.State())
	}
	if len(got) == 0 {
		t.Fatal("contiguous scan dispatched no prefetch; a stream must prefetch")
	}
	w0 := p.PeakWindow()
	p.Observe(2, 0)
	p.Observe(3, 0)
	if p.PeakWindow() <= w0 {
		t.Errorf("window did not grow across a contiguous run (%d → %d)", w0, p.PeakWindow())
	}
}

// Kernel reorder within the window (small, even negative, gaps) still counts as
// sequential — the change is about large gaps, not order (session-10 preserved).
func TestGapReorderStaysSequential(t *testing.T) {
	p := New(16)
	p.SetGapMax(bs)
	p.Open()
	p.Observe(0, 0)
	p.Observe(1, 0)
	p.Observe(2, 0)
	// A read that arrives out of order (block 1 again) within a small byte gap.
	p.Observe(1, -64<<10)
	if p.State() != Sequential {
		t.Fatalf("in-band reorder: state = %v, want Sequential", p.State())
	}
}

// A walk that becomes a stream transitions back to Sequential and grows the
// window; the detector still governs posture, the gate only gives it the byte
// information it was missing.
func TestGapWalkThenStreamRecovers(t *testing.T) {
	p := New(16)
	p.SetGapMax(bs)
	p.Open()
	p.Observe(0, 0)
	p.Observe(10, 5*bs) // seek
	p.Observe(25, 5*bs) // → Random
	if p.State() != Random {
		t.Fatalf("after scattered reads: state = %v, want Random", p.State())
	}
	// Now a contiguous run resumes.
	p.Observe(26, 0)
	p.Observe(27, 0)
	p.Observe(28, 0)
	if p.State() != Sequential {
		t.Fatalf("after a contiguous run resumed: state = %v, want Sequential", p.State())
	}
}

// With the gate off (default), a large-gap adjacent-block read is still
// sequential — the pre-1b behavior, so an un-migrated caller is unaffected.
func TestGapOffPreservesOldBehavior(t *testing.T) {
	p := New(16) // no SetGapMax → seqGapMax = MaxInt64
	p.Open()
	p.Observe(0, 0)
	p.Observe(1, 100*bs) // huge gap, but the gate is off
	if p.State() != Sequential {
		t.Fatalf("gate off: state = %v, want Sequential (pre-1b behavior)", p.State())
	}
}
