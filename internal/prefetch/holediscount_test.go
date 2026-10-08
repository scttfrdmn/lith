// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// covOf drives the coverage computation directly over a read sequence, returning the last
// coverage value. blockSize-sized reads, so each read is one block.
func covOf(t *testing.T, demanded func(off, end int64) bool, offs []int64, length int64) float64 {
	t.Helper()
	p := New(64)
	p.SetCoverage(16, 0.5)
	p.SetHoleDemanded(demanded)
	var cov float64
	for _, off := range offs {
		cov = p.recordRead(off, length)
	}
	return cov
}

// #316: a handle whose siblings' reads are absorbed by the shared page cache must not be
// held provisional by the #221 coverage gate.
//
// FOPEN_KEEP_CACHE shares the inode page cache, so one descriptor's reads never reach lith
// and every other handle sees a punctate stream with coverage ~1/N. Measured: 16 readers of
// one object take 243x longer than one reader alone, with byte amplification of 1.001 -- the
// S3 counters are the cleanest of any cell and the run is the slowest.
//
// THE DISCRIMINATION IS THE WHOLE FIX, and it is why this is not simply a lower covMin. A
// hole counts as covered only when its bytes were already DEMANDED through lith. The three
// cases below are indistinguishable by hole alignment -- all 128 KiB-aligned -- which is why
// the earlier proposal to key on alignment was wrong.
func TestHoleDiscountSeparatesASiblingsReadFromARandomWalk(t *testing.T) {
	const length = int64(128) << 10
	// One handle of four readers interleaved: it sees every 4th 128 KiB range.
	var punctate []int64
	for i := int64(0); i < 16; i++ {
		punctate = append(punctate, i*4*length)
	}

	// (1) NOBODY TOUCHED THE HOLES -- a random/strided walk. No discount, so coverage
	// stays at ~1/4 and the gate correctly holds the handle. This is #232's constraint:
	// a random mmap walk must not start prefetching.
	never := func(int64, int64) bool { return false }
	lo := covOf(t, never, punctate, length)
	if lo > 0.5 {
		t.Errorf("coverage %.3f with no hole demanded: a scattered walk would establish, "+
			"which #232 says must not happen", lo)
	}

	// (2) THE HOLES WERE ALREADY DEMANDED -- siblings read them through lith. Discounted,
	// so coverage reflects what the FILE is doing and the handle establishes.
	all := func(int64, int64) bool { return true }
	hi := covOf(t, all, punctate, length)
	if hi <= 0.5 {
		t.Errorf("coverage %.3f with every hole demanded: the #316 shape is still held by "+
			"the gate", hi)
	}

	// The two must differ across the threshold on the SAME read sequence. That is the
	// property: identical offsets, identical hole alignment, opposite verdicts, decided
	// only by whether a reader had wanted those bytes.
	if !(lo <= 0.5 && hi > 0.5) {
		t.Fatalf("the discount does not separate the cases: %.3f vs %.3f on identical "+
			"offsets", lo, hi)
	}
	t.Logf("same offsets, coverage %.3f undemanded vs %.3f demanded (covMin 0.5)", lo, hi)

	// (3) A CONTIGUOUS READER is unaffected either way -- there are no holes to discount,
	// so the discount cannot inflate a stream that was already covered.
	var seq []int64
	for i := int64(0); i < 16; i++ {
		seq = append(seq, i*length)
	}
	if c := covOf(t, never, seq, length); c < 0.99 {
		t.Errorf("contiguous coverage %.3f without the discount", c)
	}
	if c := covOf(t, all, seq, length); c < 0.99 {
		t.Errorf("contiguous coverage %.3f with the discount", c)
	}
}

// A nil predicate must reproduce the pre-#316 coverage exactly, since that is what every
// mount gets until the flag is set.
func TestHoleDiscountIsInertWhenUnset(t *testing.T) {
	const length = int64(128) << 10
	var punctate []int64
	for i := int64(0); i < 16; i++ {
		punctate = append(punctate, i*4*length)
	}
	off := covOf(t, nil, punctate, length)
	never := covOf(t, func(int64, int64) bool { return false }, punctate, length)
	if off != never {
		t.Errorf("nil predicate gives %.6f, an always-false one gives %.6f; they must be "+
			"identical or the default is not the old behaviour", off, never)
	}
	if off > 0.5 {
		t.Errorf("coverage %.3f with the discount unset: the default must still hold a "+
			"punctate handle", off)
	}
}

// Only SOME holes demanded: coverage must land between, rather than the predicate acting as
// an all-or-nothing switch. A real mount sees a mix -- a sibling has read some ranges and
// not others -- and a gate that only works at the extremes would be untrustworthy in the
// middle, which is where every real workload sits.
func TestHoleDiscountIsProportional(t *testing.T) {
	const length = int64(128) << 10
	var punctate []int64
	for i := int64(0); i < 16; i++ {
		punctate = append(punctate, i*4*length)
	}
	// Alternate holes demanded.
	var n int
	half := func(int64, int64) bool { n++; return n%2 == 0 }
	mid := covOf(t, half, punctate, length)
	lo := covOf(t, func(int64, int64) bool { return false }, punctate, length)
	hi := covOf(t, func(int64, int64) bool { return true }, punctate, length)
	t.Logf("coverage: none %.3f, alternate %.3f, all %.3f", lo, mid, hi)
	if !(mid > lo && mid < hi) {
		t.Errorf("alternate-hole coverage %.3f is not between %.3f and %.3f; the predicate "+
			"is behaving as a switch rather than a measure", mid, lo, hi)
	}
}
