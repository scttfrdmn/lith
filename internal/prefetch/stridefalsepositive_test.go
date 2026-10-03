// SPDX-License-Identifier: Apache-2.0

package prefetch

import (
	"math/rand"
	"testing"
)

// #222: the stride branch's only evidence is a repeated block delta, and one repeat fires on
// coincidence.
//
// It has no byte-gap bound (unlike the sequential branch, which requires
// absInt64(byteGap) <= seqGapMax) and no coverage check, and it runs before the seek rule on
// purpose, because a large gap is what a stride IS. So over a walk spanning B blocks,
// consecutive deltas collide by chance with probability ~1/B, and N reads yield ~N/B false
// strides -- each one declaring a pattern and dispatching a prefetch.
//
// That was measured on real hardware: a genuinely random mmap walk flipped Random -> Strided
// 7 times in 2471 reads on gaps of +55 MB, +166 MB and +131 MB, dispatching prefetch for
// +27 MB over distinct (#232's 5f-P11). This package's own harness had been reporting
// strided=1 and strided=3 over 281 and 1126 random reads before anyone went looking.
//
// This is that arithmetic, run deterministically.
func TestStrideDoesNotFireOnARandomWalk(t *testing.T) {
	const blocks = 109 // 876 MB / 8 MiB, the measured object
	const reads = 2471 // the measured read count
	const blockSize = int64(8) << 20

	// Count false strides over several seeds, so the result is not one lucky walk.
	var total, worst int
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := New(223)
		p.SetGapMax(blockSize)
		p.SetCoverage(16, 0.5)

		flips := 0
		prev := State(-1)
		for i := 0; i < reads; i++ {
			blk := rng.Int63n(blocks)
			p.Observe(blk, blk*blockSize, 4096, blk*blockSize)
			if st := p.State(); st == Strided && prev != Strided {
				flips++
			}
			prev = p.State()
		}
		total += flips
		if flips > worst {
			worst = flips
		}
	}

	t.Logf("%d random reads over %d blocks, 8 seeds: %d Strided flips total, worst seed %d "+
		"(predicted ~%d per seed at one repeat, ~%.2f at two)",
		reads, blocks, total, worst, reads/blocks, float64(reads)/float64(blocks)/float64(blocks))

	// At one repeat the prediction is ~22 per seed, ~180 over eight. At two repeats it is
	// ~0.2 per seed. Allowing a handful across all eight seeds leaves room for the tail
	// without admitting the old rate.
	if total > 8 {
		t.Errorf("%d Strided flips across 8 random walks (worst seed %d): the stride branch "+
			"is still concluding a pattern from coincidence. At one repeat this was ~%d per "+
			"seed; strideRun is meant to make it ~0.2.", total, worst, reads/blocks)
	}
}

// The fix must not break the branch for its legitimate users: a real strided reader has to
// still establish, and still dispatch its prediction. It just takes one read longer.
func TestStrideStillEstablishesForARealStride(t *testing.T) {
	const blockSize = int64(8) << 20
	for _, delta := range []int64{2, 3, 8, 17} {
		p := New(223)
		p.SetGapMax(blockSize)
		p.SetCoverage(16, 0.5)

		var dispatched int
		for i := int64(0); i < 24; i++ {
			blk := i * delta
			d := p.Observe(blk, blk*blockSize, 4096, blk*blockSize)
			dispatched += len(d)
		}
		if p.State() != Strided {
			t.Errorf("delta %d: state = %v after 24 evenly-strided reads, want Strided",
				delta, p.State())
		}
		// It predicts one block ahead per read once established, so a 24-read walk that
		// confirms on the fourth read should dispatch around twenty.
		if dispatched < 15 {
			t.Errorf("delta %d: dispatched %d predictions over 24 strided reads, want >= 15 — "+
				"the run requirement has broken the branch for a real stride", delta, dispatched)
		}
	}
}

// A near-stride that is interrupted must not accumulate across the interruption. Without
// resetting the run, a match at delta X, then a non-match, then two matches at delta Y would
// reach strideRun on three reads that were never consecutive.
func TestStrideRunDoesNotAccumulateAcrossAnInterruption(t *testing.T) {
	const blockSize = int64(8) << 20
	p := New(223)
	p.SetGapMax(blockSize)
	p.SetCoverage(16, 0.5)

	obs := func(blk int64) []int64 {
		return p.Observe(blk, blk*blockSize, 4096, blk*blockSize)
	}
	obs(0)
	obs(5)  // delta 5
	obs(10) // delta 5 repeats once
	obs(40) // delta 30 — breaks the run
	obs(70) // delta 30 repeats once: must NOT be enough, even though two matches have
	//         now occurred in total
	if p.State() == Strided {
		t.Errorf("state = Strided after two non-consecutive delta matches: the run is " +
			"accumulating across an interruption, so the false-positive rate is still ~1/B")
	}
	obs(100) // delta 30 repeats twice -> now it may establish
	if p.State() != Strided {
		t.Errorf("state = %v after three consecutive delta-30 reads, want Strided", p.State())
	}
}
