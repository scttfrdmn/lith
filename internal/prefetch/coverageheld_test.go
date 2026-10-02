// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #316: a handle making CONTIGUOUS progress that the coverage gate refuses must be counted.
//
// This is the shape where concurrent readers share one object: the kernel page cache serves
// each handle's siblings' reads, so a handle sees every Nth read of a dense scan. Its offsets
// advance in order, well inside the byte-gap bound, but its trailing coverage is ~1/N.
//
// Nothing counted it. `lowCoverage` increments only on the SEEK path, and `deEstablish()`
// counts only an establishment that existed — these handles are refused while still
// provisional — so the purest #316 cell incremented ZERO counters. An external 48-rank
// deployment found that by reading the code after the metric went in.
func TestCoverageHeldCountsAProvisionalContiguousRejection(t *testing.T) {
	const blockSize = int64(8) << 20 // the shipping default
	const readLen = int64(128) << 10 // the kernel readahead unit, which is the hole size
	p := New(223)
	p.SetGapMax(blockSize)
	p.SetCoverage(16, 0.5)

	// Four readers sharing one object. This handle sees one read in four; the kernel served
	// the other three from the shared page cache. The holes VARY -- the reported ones were
	// 128K > 256K > 384K > 512K -- which matters: a CONSTANT stride is classified Strided and
	// takes a different branch entirely, where this counter is also absent. A first version of
	// this test used a fixed 8-block stride, landed in Strided, and proved nothing.
	steps := []int64{4, 3, 5, 4, 2, 6, 4, 3} // in readLen units, mean 3.875
	var off int64
	for i := 0; i < 48; i++ {
		blk := off / blockSize
		p.Observe(blk, off, readLen, 0)
		off += steps[i%len(steps)] * readLen
	}

	held, seek := p.CoverageHeld(), p.LowCoverage()
	t.Logf("48 reads at ~1/3.9 coverage, varying 128-768 KiB holes: coverage_held=%d "+
		"low_coverage=%d state=%v window=%d", held, seek, p.State(), p.Window())

	if p.State() == Sequential {
		t.Fatalf("fixture: the handle established, so the gate never fired and this test "+
			"proves nothing (state %v)", p.State())
	}
	if held == 0 {
		t.Error("a handle advancing in order at ~1/4 coverage was refused a window and " +
			"CoverageHeld is 0 — the #316 shape is still uncounted")
	}
	// It ticks at BLOCK CROSSINGS, not per read: 64 of these 128 KiB reads fit in one 8 MiB
	// block, and a read landing in the block the cursor is already on does not reach the gate.
	// So the counter's magnitude is "block boundaries refused", roughly reads x readLen /
	// blockSize, which is why 48 reads here produce a handful rather than 48. That is the
	// right denominator to have in mind when reading the series -- a non-zero value means the
	// handle is being refused at every boundary it reaches, which is total starvation.
	if held > 48 {
		t.Errorf("CoverageHeld = %d over 48 reads: it is counting something per-read rather "+
			"than per block boundary", held)
	}
	if p.Window() != 0 {
		t.Errorf("window = %d, want 0: a provisional handle must dispatch nothing", p.Window())
	}
}

// A dense sequential stream must trip NEITHER counter, or the signal is worthless: #316 is
// diagnosed by the held counter rising, and a normal reader raising it would bury that.
func TestCoverageHeldIsSilentOnADenseStream(t *testing.T) {
	const blockSize = int64(1) << 20
	p := New(223)
	p.SetGapMax(blockSize)
	p.SetCoverage(16, 0.5)

	for i := int64(0); i < 64; i++ {
		p.Observe(i, i*blockSize, blockSize, 0)
	}
	if p.State() != Sequential {
		t.Fatalf("fixture: a 64-block contiguous scan did not establish (state %v)", p.State())
	}
	// The first two reads are below covReads=3, so the gate cannot judge and the handle is
	// provisional — a small count there is the ramp, not a rejection. Bound it rather than
	// demanding zero.
	if got := p.CoverageHeld(); got > 3 {
		t.Errorf("a dense 64-block stream raised coverage_held %d times, want <= 3 (the "+
			"pre-judgement ramp only)", got)
	}
	if got := p.LowCoverage(); got != 0 {
		t.Errorf("a dense stream tripped the SEEK coverage counter %d times, want 0", got)
	}
}
