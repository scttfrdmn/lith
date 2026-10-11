// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #407: the evidence gate's input is CUMULATIVE over a handle's life, so a
// scattered-but-locally-sequential walk earns a window no single run can justify.
//
// The shape, from an external sweep: a GRIB .idx field sweep reads 12 fields of 0.5-2 MB, each
// a legitimate short sequential run separated by a seek. Measured there: 48 chunks prefetched
// and 2 used, with a peak window of 5 blocks. The arithmetic reproduces that exactly --
// 11.9 MB consumed at ratio 4 over an 8 MiB block earns 5.69 -> 5 -- which is what identified
// cumulative consumption as the mechanism.
//
// This test is the local version of that shape, and it asserts the WINDOW rather than bytes,
// because the window is what the gate decides and the bytes are the FUSE layer's consequence.
func TestPerRunEvidenceBoundsWhatCumulativeDoesNot(t *testing.T) {
	const blockBytes = int64(8) << 20
	const ratio = 4.0

	// THE DECISION, TESTED DIRECTLY, with the figures an external trace of the real shape
	// implies: a GRIB .idx sweep over a 507 MB file took 91 reads of 128 KiB (11.9 MB
	// cumulative) in runs of one field each (0.5-2 MB), and its peak window was 5 blocks.
	//
	// WHY NOT A FULL WALK FIXTURE. I built one and it could not reproduce the ramp: with
	// honest byte gaps the jumps alternate re-anchor and pending->Random, so the handle never
	// stays Sequential long enough to double its window, and BOTH arms sat at the 2-block
	// floor. The real trace is 67 random->random transitions against 12 sequential ones, and
	// reproducing that state machine locally is reproducing the walk rather than testing the
	// gate. The precondition in that fixture caught it rather than letting it pass -- but the
	// honest conclusion is that the EMERGENT behaviour has to be measured on the real shapes,
	// which the reporter's sweep harness does in one run.
	//
	// So this tests what windowCap decides, given the two bases. That is the whole of the
	// change, and it is the same separation floorIndex, regionPairFrom and pressureMaxFor use.
	cap := func(consumed, runConsumed int64, perRun bool) int64 {
		p := New(64)
		p.SetEvidence(ratio, blockBytes)
		p.SetPerRunEvidence(perRun)
		p.consumed, p.runConsumed = consumed, runConsumed
		return p.windowCap()
	}

	const lifetime = int64(91) * (128 << 10) // 11.9 MB, the measured cumulative total
	const oneRun = int64(1) << 20            // one GRIB field

	cumulative := cap(lifetime, oneRun, false)
	perRun := cap(lifetime, oneRun, true)
	t.Logf("consumed %d MB lifetime / %d MB this run, ratio %g, %d MiB blocks:",
		lifetime>>20, oneRun>>20, ratio, blockBytes>>20)
	t.Logf("  cumulative basis -> cap %d blocks", cumulative)
	t.Logf("  per-run basis    -> cap %d blocks", perRun)

	// REPRODUCES THE EXTERNALLY MEASURED PEAK WINDOW. 11.9 MB x 4 / 8 MiB = 5.69 -> 5, and
	// the sweep measured 5. If this stops matching, either the arithmetic or the measurement
	// has moved and the mechanism needs re-deriving before anything is built on it.
	if cumulative != 5 {
		t.Errorf("cumulative basis gives %d blocks; the external trace measured a peak window "+
			"of 5 from this exact consumption, so the mechanism no longer reproduces it",
			cumulative)
	}
	// THE CHANGE: one 1 MiB run earns 4 MiB, under one 8 MiB block, so it floors.
	if perRun != initialWindow {
		t.Errorf("per-run basis gives %d blocks, want the %d-block floor: a %d MiB run earns "+
			"%d MiB at ratio %g", perRun, initialWindow, oneRun>>20,
			int64(float64(oneRun)*ratio)>>20, ratio)
	}

	// AND THE LIMIT OF THIS ARM, asserted so it cannot be forgotten: the floor is 2 blocks,
	// which at the default 8 MiB block is 17 MB against runs of 0.5-2 MB. Per-run evidence
	// cannot take a short run below that, so it is necessary and NOT sufficient -- the second
	// arm (sub-block dispatch) is what the remaining over-fetch needs.
	if floorBytes := initialWindow * blockBytes; floorBytes <= 4*oneRun {
		t.Errorf("the %d-block floor is %d MB, not above this arm's reach for a %d MB run; "+
			"the sufficiency note in windowCap needs revisiting", initialWindow,
			floorBytes>>20, oneRun>>20)
	} else {
		t.Logf("  floor is %d blocks = %d MB, against a %d MB run: this arm alone cannot "+
			"close the gap", initialWindow, floorBytes>>20, oneRun>>20)
	}

	// A long run is unaffected: once a run has consumed enough, both bases agree.
	if a, b := cap(lifetime, lifetime, false), cap(lifetime, lifetime, true); a != b {
		t.Errorf("with run == lifetime the two bases disagree (%d vs %d)", a, b)
	}
}

// ★ THE WITNESS ANY FIX HERE MUST NOT BREAK: a genuine sequential copy never seeks, so its run
// is the whole file and per-run evidence must be a NO-OP for it.
//
// This is the witness #231 refuted three separate cold-start fixes against, and it is the
// reason the reset is placed at run boundaries rather than on a timer or a read count: a copy
// crosses no run boundary, so there is nothing to reset.
func TestPerRunEvidenceDoesNotTouchASequentialCopy(t *testing.T) {
	const blockBytes = int64(8) << 20
	const readLen = int64(128) << 10
	const ratio = 4.0

	copyRun := func(perRun bool) (peak, dispatched int64) {
		p := New(64)
		p.SetGapMax(blockBytes)
		p.SetEvidence(ratio, blockBytes)
		p.SetPerRunEvidence(perRun)
		dispatched = int64(len(p.Open()))
		// 256 MiB read straight through, contiguously: gap 0 is TRUE here, because each read
		// starts exactly where the last ended. No seeks, so no run boundaries.
		for off := int64(0); off < 256<<20; off += readLen {
			dispatched += int64(len(p.Observe(off/blockBytes, off, readLen, 0)))
		}
		return p.PeakWindow(), dispatched
	}

	cumPeak, cumDisp := copyRun(false)
	runPeak, runDisp := copyRun(true)
	t.Logf("256 MiB contiguous copy: cumulative peak %d / %d dispatched; per-run peak %d / %d",
		cumPeak, cumDisp, runPeak, runDisp)

	if runPeak != cumPeak {
		t.Errorf("per-run changed a sequential copy's peak window: %d against %d. A copy never "+
			"seeks, so its run is the whole file and this must be a no-op for it", runPeak, cumPeak)
	}
	if runDisp != cumDisp {
		t.Errorf("per-run changed a sequential copy's dispatch volume: %d against %d",
			runDisp, cumDisp)
	}
	// And the copy must actually have earned a real window, or this proves nothing.
	if cumPeak <= initialWindow {
		t.Fatalf("the copy peaked at %d blocks, the floor is %d: the fixture is not ramping",
			cumPeak, initialWindow)
	}
}

// The reset happens at run boundaries, and the seek re-anchor is one even though it keeps the
// handle Sequential. Without that, per-run evidence would be identical to cumulative evidence
// for exactly the shape it exists for.
func TestPerRunResetsAtTheSeekReanchorNotOnlyOnDeEstablish(t *testing.T) {
	const blockBytes = int64(8) << 20
	const readLen = int64(128) << 10
	p := New(64)
	p.SetGapMax(blockBytes)
	p.SetEvidence(4.0, blockBytes)
	p.SetPerRunEvidence(true)
	p.Open()

	// Build up a run of 8 MiB so it has earned more than the floor. Gaps are 0 here and that
	// is TRUE -- each read starts where the last ended.
	off := int64(0)
	for ; off < 8<<20; off += readLen {
		p.Observe(off/blockBytes, off, readLen, 0)
	}
	if p.runConsumed == 0 {
		t.Fatal("run consumption did not accumulate")
	}
	if p.State() != Sequential {
		t.Fatalf("state %v, want Sequential before the seek", p.State())
	}

	// A seek out of the established pattern: Sequential is KEPT (the #49 re-anchor), so
	// deEstablish is never called — only endRun is.
	// 80 MiB on, with the TRUE byte gap: outside both the reorder band and seqGapMax, so it
	// is unambiguously a seek. Passing gap 0 here is what made the walk fixture above read
	// its jumps as sequential progress.
	prevEnd := off
	off += 80 << 20
	p.Observe(off/blockBytes, off, readLen, off-prevEnd)
	if p.State() != Sequential {
		t.Fatalf("state %v after the re-anchor, want Sequential — this test is not exercising "+
			"the re-anchor branch", p.State())
	}
	if p.runConsumed > readLen {
		t.Errorf("runConsumed is %d after a seek re-anchor; the run should have been reset and "+
			"hold only the landing read (%d)", p.runConsumed, readLen)
	}
}
