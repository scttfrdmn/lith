// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #221: Sequential requires coverage — a stream tiles its span, a scattered walk
// leaves most of it untouched. The gate keys on the coverage ratio over a
// trailing window, not on block proximity or gap magnitude alone.

const cbs = 8 << 20 // block size used to map offsets to block indices in these tests

// stream reads block i as a contiguous cbs-byte read at offset i*cbs.
func streamRead(p *Prefetcher, i int64) []int64 {
	return p.Observe(i, i*cbs, cbs, 0)
}

func TestCoverage_TilingStreamStaysSequential(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	for i := int64(0); i < 40; i++ {
		streamRead(p, i)
	}
	if p.State() != Sequential {
		t.Fatalf("a contiguous tiling stream must stay Sequential, got %v", p.State())
	}
	if p.LowCoverage() != 0 {
		t.Fatalf("a stream must never trip the coverage gate, tripped %d times", p.LowCoverage())
	}
}

func TestCoverage_ScatteredWalkGoesRandom(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	// 4 KiB reads at offsets megabytes apart with sub-block *and* super-block gaps:
	// the GRIB/HDF5 metadata-walk shape. Block deltas look sequential-ish but the
	// span is huge and coverage collapses.
	offs := []int64{0, 30 * cbs, 15 * cbs, 28 * cbs, 3 * cbs, 22 * cbs, 9 * cbs, 31 * cbs}
	for _, o := range offs {
		p.Observe(o/cbs, o, 4<<10, o) // byteGap ~= offset (scattered)
	}
	if p.State() != Random {
		t.Fatalf("a scattered low-coverage walk must classify Random, got %v", p.State())
	}
	if p.LowCoverage() == 0 {
		t.Fatal("the coverage gate must have fired on the walk")
	}
}

func TestCoverage_FieldWalkGoesRandom(t *testing.T) {
	// The GRIB .idx shape as the #221 characterization actually measured it: each
	// field is a short contiguous run of block reads, and fields are *megabytes*
	// apart — so the jumps between fields hit the seek path (not sub-block gaps, as
	// the pre-measurement framing assumed). Within a field the M16 gap rule sees
	// d==1 and re-anchors Sequential; without coverage the cross-field jump would
	// re-anchor again and prefetch. Coverage sees the low fill of the huge span and
	// classifies Random.
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	// 12 fields, each 2 contiguous full-block reads, spaced ~40 blocks apart. The
	// cross-field jump must be gated (Random, no prefetch); the intra-field
	// contiguous read legitimately re-establishes Sequential and reads the field —
	// so the win is that the jumps stop refilling across the file, not that the
	// handle never prefetches a field it is actually reading.
	p.Observe(0, 0, cbs, 0) // field 0: establish a stream (the initial ramp)
	p.Observe(1, cbs, cbs, 0)
	for f := int64(1); f < 12; f++ {
		base := f * 40 * cbs
		disp := p.Observe(base/cbs, base, cbs, base) // jump to field f (seek)
		if len(disp) != 0 {
			t.Fatalf("field-jump %d prefetched %v; a low-coverage seek must dispatch nothing", f, disp)
		}
		if p.State() != Random {
			t.Fatalf("field-jump %d must land Random, got %v", f, p.State())
		}
		p.Observe(base/cbs+1, base+cbs, cbs, 0) // second block of the field (contiguous, legitimate)
	}
	if p.LowCoverage() < 11 {
		t.Fatalf("every cross-field jump must trip the coverage gate; got %d of 11", p.LowCoverage())
	}
}

func TestCoverage_ReorderedReadsStaySequential(t *testing.T) {
	// Kernel reorder within the readahead band: blocks arrive 0,2,1,3,5,4,6,...
	// They still tile (full-block reads), so coverage stays high and the handle
	// stays Sequential — reorder tolerance falls out of coverage, not a special case.
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	order := []int64{0, 2, 1, 3, 5, 4, 6, 8, 7, 9, 11, 10, 12, 14, 13, 15}
	for _, b := range order {
		gap := int64(0) // reorder within band: near-contiguous, small gaps
		p.Observe(b, b*cbs, cbs, gap)
	}
	if p.State() != Sequential {
		t.Fatalf("reordered-but-tiling reads must stay Sequential, got %v", p.State())
	}
}

func TestCoverage_WalkBecomesStreamTransitions(t *testing.T) {
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	// First a scattered walk -> Random.
	offs := []int64{0, 40 * cbs, 12 * cbs, 33 * cbs, 7 * cbs, 25 * cbs}
	for _, o := range offs {
		p.Observe(o/cbs, o, 4<<10, o)
	}
	if p.State() != Random {
		t.Fatalf("setup: walk should be Random, got %v", p.State())
	}
	// Then a contiguous stream from block 100. Once the window refills with tiling
	// reads, coverage recovers and the handle returns to Sequential.
	var got []int64
	for i := int64(100); i < 130; i++ {
		got = p.Observe(i, i*cbs, cbs, 0)
	}
	if p.State() != Sequential {
		t.Fatalf("a walk that becomes a stream must transition back to Sequential, got %v", p.State())
	}
	if len(got) == 0 {
		t.Fatal("prefetch must resume once the handle is Sequential again")
	}
}

func TestCoverage_SeekReanchorSuppressedOnLowCoverage(t *testing.T) {
	// The specific #220 mechanism: after an established Sequential run, a far seek
	// whose trailing coverage is low must NOT re-anchor Sequential + prefetch two
	// blocks (which refilled whole objects). It must go Random and dispatch nothing.
	p := New(64)
	p.SetGapMax(cbs)
	p.SetCoverage(16, 0.5)
	p.Open()
	for i := int64(0); i < 20; i++ { // establish a stream
		streamRead(p, i)
	}
	if p.State() != Sequential {
		t.Fatalf("setup: expected Sequential, got %v", p.State())
	}
	// A cluster of far, tiny reads that drags coverage under threshold.
	var last []int64
	for _, o := range []int64{200 * cbs, 260 * cbs, 230 * cbs, 290 * cbs, 210 * cbs} {
		last = p.Observe(o/cbs, o, 4<<10, o)
	}
	if p.State() != Random {
		t.Fatalf("low-coverage landings must suppress the seek re-anchor (Random), got %v", p.State())
	}
	if len(last) != 0 {
		t.Fatalf("a suppressed re-anchor must dispatch no prefetch, got %v", last)
	}
}

func TestCoverage_DisabledIsBehaviorPreserving(t *testing.T) {
	// With the gate off (default), a scattered walk behaves as before (the M16 gap
	// rule still demotes on large gaps, but the coverage gate itself never fires).
	p := New(64)
	p.SetGapMax(cbs)
	p.Open()
	for _, o := range []int64{0, 30 * cbs, 15 * cbs, 28 * cbs} {
		p.Observe(o/cbs, o, 4<<10, o)
	}
	if p.LowCoverage() != 0 {
		t.Fatalf("coverage gate must not fire when disabled, fired %d", p.LowCoverage())
	}
}
