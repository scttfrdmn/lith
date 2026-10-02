// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sync/atomic"
	"testing"
)

// THE DEFECT THIS PINS, in the form it was found (#316, #319).
//
// Every per-handle prefetch counter used to be folded into its metric once, at Release. An
// external 48-rank deployment holding its met handles open for the whole run therefore saw
// ZERO in every 2 Hz sample, and non-zero values (527, 522) only in the final scrape — after
// the job was killed and its handles closed. A counter that cannot be read until the workload
// ends cannot diagnose the workload, and that is how two defects stayed invisible.
//
// So: a read must produce its deltas from `observe` itself, with no Release anywhere. If
// someone moves this bookkeeping back to close, this fails.
func TestCountersAreProducedPerReadNotAtRelease(t *testing.T) {
	const blockSize = int64(8) << 20
	const readLen = int64(128) << 10
	var seq atomic.Int64

	// A handle whose own stream is punctate — the #316 shape, where the shared page cache has
	// absorbed the sibling reads of a dense scan. Irregular holes, because a CONSTANT stride
	// lands in the Strided branch instead.
	w := newPFWrapper(223, blockSize, 0, coverageMin)
	steps := []int64{4, 3, 5, 4, 2, 6, 4, 3}
	var off int64
	var total int64
	for i := 0; i < 64; i++ {
		obs := w.observe(off/blockSize, off, off+readLen, 223, 0, &seq)
		total += obs.counters.CoverageHeld
		off += steps[i%len(steps)] * readLen
	}
	if total == 0 {
		t.Error("64 reads of a punctate handle produced no CoverageHeld delta from observe(): " +
			"the counter is only reachable at Release again, which is the #316 defect")
	}

	// A seeking handle moves the other coverage counter and the reset counter, again from the
	// read itself.
	w2 := newPFWrapper(223, blockSize, 0, coverageMin)
	var seeks, resets int64
	for i := int64(0); i < 48; i++ {
		// Dense enough to establish, then a far jump, repeatedly.
		base := i * 997 * blockSize
		for j := int64(0); j < 4; j++ {
			o := base + j*blockSize
			obs := w2.observe(o/blockSize, o, o+blockSize, 223, 0, &seq)
			seeks += obs.counters.Seek
			resets += obs.counters.Resets
		}
	}
	if seeks == 0 && resets == 0 {
		t.Error("a repeatedly-seeking handle produced neither a Seek nor a Resets delta from " +
			"observe()")
	}

	// THE EVIDENCE GATE's own counters, which is the pair that matters for the work this
	// phase is plumbing: if the gate's default is ever flipped, its action has to be visible
	// on a running job or there is no way to tell a window the gate refused from one the
	// detector never wanted.
	w3 := newPFWrapper(223, blockSize, 0, coverageMin)
	var held, withheld int64
	for i := int64(0); i < 32; i++ {
		o := i * blockSize
		// FULL-block contiguous reads, deliberately. Small reads do not reach the evidence
		// gate at all: their coverage is ~1/64, so the #221 coverage gate holds the handle
		// provisional and returns before the ramp ever consults windowCap(). A dense scan
		// establishes, enters the ramp, and THEN under-earns — after n reads it has consumed
		// n blocks and earned 4n against a 223-block max, so the gate holds for n < 56.
		// Ratio passed per read, which is the other half of this phase.
		obs := w3.observe(i, o, o+blockSize, 223, 4, &seq)
		held += obs.counters.EvidenceHeld
		withheld += obs.counters.EvidenceWithheld
	}
	if held == 0 {
		t.Error("the evidence gate held a window 0 times from observe() with ratio 4 and " +
			"128 KiB reads — its action is invisible on a running job")
	}
	if withheld == 0 {
		t.Errorf("EvidenceHeld is %d but EvidenceWithheld is 0: the blocks-withheld counter "+
			"is not being produced per read", held)
	}
}

// The deltas must be DELTAS, not the cumulative totals. Recording cumulative values per read
// would make every counter grow quadratically in reads — a failure that looks like a working
// metric until someone reads the absolute number.
func TestCountersAreDeltasNotTotals(t *testing.T) {
	const blockSize = int64(8) << 20
	var seq atomic.Int64
	w := newPFWrapper(223, blockSize, 0, coverageMin)

	// Ratio 4 and a dense scan: the handle establishes and then under-earns for its first ~56
	// reads, so the cumulative total climbs steadily and a delta must stay small and bounded.
	var maxDelta int64
	for i := int64(0); i < 48; i++ {
		o := i * blockSize
		obs := w.observe(i, o, o+blockSize, 223, 4, &seq)
		if obs.counters.EvidenceHeld > maxDelta {
			maxDelta = obs.counters.EvidenceHeld
		}
	}
	// One read can hold the window at most a couple of times (windowCap is consulted on the
	// ramp). Anything approaching the read count means totals are being reported as deltas.
	if maxDelta > 4 {
		t.Errorf("largest single-read EvidenceHeld delta is %d over 64 reads: the snapshot "+
			"diff is reporting cumulative totals, not deltas", maxDelta)
	}
	if maxDelta == 0 {
		t.Fatal("fixture: the evidence gate never held, so this proves nothing")
	}
}
