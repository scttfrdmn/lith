// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// ★★★ THE TEST THAT SHOULD HAVE EXISTED BEFORE THE ARM WAS DEFAULTED ON (#407).
//
// A sub-block span bounds the dispatch a handle gets at establishment. If it is placed once and
// never extended, a handle that goes on STREAMING faults through everything the span did not
// cover, because the whole-block frontier cannot move again until the reader crosses a block --
// 64 reads at the default 8 MiB block and a 128 KiB read. That shipped, and an external
// bracketed cell measured an index-then-stream reader 20% slower with bytes IDENTICAL: the
// entire cost was faults and wall.
//
// ★ AND THIS IS THE AXIS A LOCAL FIXTURE CAN ACTUALLY GATE. The earlier witness for the same
// risk asserted BYTES, which are identical, and the fault counters are too noisy in a fake-server
// fixture to assert on (+5/+1/+0 and +4/+4/+11 across three identical runs, because UncoveredMiss
// races the prefetch goroutine). DISPATCH COVERAGE in byte space is deterministic -- it is a
// property of the decisions, not of the timing -- so it is what this asserts.
//
// ★ IT SWEEPS THE STREAM'S PHASE relative to the block grid, and that is not decoration. At a
// block-aligned start the handle establishes 64 reads in, has earned a near-floor span, and the
// gap does not appear at all: a first version of this probe used an aligned base and reported the
// two arms IDENTICAL, which reads exactly like "no regression". The gap is largest when the
// handle establishes EARLY, having earned little:
//
//	phase   established at   uncovered, whole-block   uncovered, span placed once
//	+0 MiB  read 64          65                       65   (invisible)
//	+2 MiB  read 48          49                       73
//	+6 MiB  read 16          17                       105  (6x)
//	+7 MiB  read 72          73                       73   (invisible)
//
// With the lead maintained, every phase matches the whole-block arm.
func TestSpanLeadsAStreamingReaderAtEveryPhase(t *testing.T) {
	const bs = int64(8) << 20
	const rd = int64(128) << 10
	const chunk = int64(1) << 20
	const streamLen = int64(64) << 20

	// uncoveredReads replays a scatter-then-stream handle and counts stream reads that no
	// dispatch had covered when they arrived.
	uncoveredReads := func(phase int64, sub bool) (uncovered, reads, estAt int) {
		p := New(223)
		p.SetGapMax(bs)
		p.SetCoverage(16, 0.5)
		p.SetEvidence(2.0, bs)
		p.SetSubBlockDispatch(sub)
		p.Open()

		covered := map[int64]bool{}
		mark := func(off, length int64) {
			for c := off / chunk; c <= (off+length-1)/chunk; c++ {
				covered[c] = true
			}
		}
		apply := func(ds []Dispatch) {
			for _, d := range ds {
				if d.SpanLen > 0 {
					mark(d.SpanOff, d.SpanLen)
					continue
				}
				mark(d.Block*bs, bs)
			}
		}

		prevEnd := int64(-1)
		for i := int64(0); i < 6; i++ { // scatter: far apart, collapses to Random
			off := i * 37 << 20
			gap := int64(0)
			if prevEnd >= 0 {
				gap = off - prevEnd
			}
			apply(p.Observe(off/bs, off, rd, gap))
			prevEnd = off + rd
		}
		if p.Resets() == 0 {
			t.Fatalf("phase %d: the scatter phase never collapsed to Random, so this handle has "+
				"no Random history and the arm under test cannot fire", phase>>20)
		}

		base := int64(400)<<20 + phase
		estAt = -1
		for i := int64(0); i*rd < streamLen; i++ {
			off := base + i*rd
			ds := p.Observe(off/bs, off, rd, off-prevEnd)
			prevEnd = off + rd
			reads++
			if !covered[off/chunk] {
				uncovered++
			}
			apply(ds)
			if p.Established() && estAt < 0 {
				estAt = int(i)
			}
		}
		return uncovered, reads, estAt
	}

	worst := 0
	for _, phase := range []int64{0, 2 << 20, 4 << 20, 6 << 20, 7 << 20} {
		blockU, reads, blockEst := uncoveredReads(phase, false)
		spanU, _, spanEst := uncoveredReads(phase, true)
		t.Logf("phase +%d MiB: established at read %d/%d; uncovered whole-block %3d/%d, span %3d/%d",
			phase>>20, blockEst, spanEst, blockU, reads, spanU, reads)

		if blockEst != spanEst {
			t.Errorf("phase +%d MiB: the arms established at different reads (%d vs %d), so this "+
				"comparison is between two different runs", phase>>20, blockEst, spanEst)
		}
		// THE ASSERTION. A span bounds how much is COMMITTED; it must not reduce what is
		// COVERED for a reader that keeps going. Equality, which is sound here because dispatch
		// coverage is a function of the decisions and not of timing.
		if spanU > blockU {
			t.Errorf("phase +%d MiB: %d of %d stream reads were uncovered with sub-block "+
				"dispatch against %d with whole blocks. The span is not keeping its lead, so a "+
				"handle that scatters and then STREAMS faults through the remainder -- the "+
				"regression that was measured at 20%% slower wall with bytes identical",
				phase>>20, spanU, reads, blockU)
		}
		if d := spanU - blockU; d > worst {
			worst = d
		}
	}
	t.Logf("worst extra uncovered reads across phases: %d", worst)

	// AND THE SWEEP MUST CONTAIN A PHASE WHERE THE GAP WOULD BE VISIBLE, or it proves nothing.
	// An early establishment is what makes the span small; if no phase establishes early, a
	// future change to the detector has moved this fixture out of the regime it tests.
	early := false
	for _, phase := range []int64{0, 2 << 20, 4 << 20, 6 << 20, 7 << 20} {
		if _, _, est := uncoveredReads(phase, true); est >= 0 && est < 32 {
			early = true
		}
	}
	if !early {
		t.Error("no phase in this sweep establishes within 32 reads, so none of them produces " +
			"the small span that the regression came from — the fixture no longer covers the " +
			"case it was written for")
	}
}

// THE LEAD MUST BE MAINTAINED WITHOUT RE-DISPATCHING WHAT IT ALREADY COVERS.
//
// Extending a span on every read would also close the coverage gap, so the coverage test above
// cannot tell a frontier from no frontier -- a revert sweep confirmed that removing the frontier
// check left every test green. The cost is not bytes (the blockstore dedups a range it has
// already fetched) but one dispatch and one goroutine per read: ~8200 of them over a 1 GiB
// stream instead of a few dozen.
//
// Two properties, both measured on the streaming phase: dispatches are a small fraction of
// reads, and no dispatch starts behind where the previous one ended.
func TestSpanDispatchesAreBoundedAndNonOverlapping(t *testing.T) {
	const bs = int64(8) << 20
	const rd = int64(128) << 10

	p := New(223)
	p.SetGapMax(bs)
	p.SetCoverage(16, 0.5)
	p.SetEvidence(2.0, bs)
	p.SetSubBlockDispatch(true)
	p.Open()

	prevEnd := int64(-1)
	for i := int64(0); i < 6; i++ { // scatter -> Random
		off := i * 37 << 20
		g := int64(0)
		if prevEnd >= 0 {
			g = off - prevEnd
		}
		p.Observe(off/bs, off, rd, g)
		prevEnd = off + rd
	}

	// Phase +6 MiB: establishes early, so the span is small and the lead has the most work to do.
	base := int64(400)<<20 + 6<<20
	var spans, reads, overlaps int
	lastEnd := int64(-1)
	for i := int64(0); i*rd < 64<<20; i++ {
		off := base + i*rd
		ds := p.Observe(off/bs, off, rd, off-prevEnd)
		prevEnd = off + rd
		reads++
		for _, d := range ds {
			if d.SpanLen <= 0 {
				continue
			}
			spans++
			if lastEnd >= 0 && d.SpanOff < lastEnd {
				overlaps++
			}
			lastEnd = d.SpanOff + d.SpanLen
		}
	}
	t.Logf("%d stream reads -> %d span dispatches (%.1f%% of reads), %d overlapping",
		reads, spans, 100*float64(spans)/float64(reads), overlaps)

	if spans == 0 {
		t.Fatal("no spans were dispatched over the streaming phase, so this measures nothing")
	}
	// A GENEROUS BOUND, not a tuned one: measured ~9%, and a per-read dispatch would be ~100%.
	//
	// HONEST NOTE ON WHAT HOLDS IT: not the frontier's early return. A revert sweep removing
	// that check left this figure at 48 of 512, unchanged -- because `earned` grows twice as
	// fast as the reader advances, so nearly every eligible read genuinely extends the lead,
	// and the arm stops entirely once earned reaches the 2-block floor (~51 reads in). The
	// early return is defensive and no measured shape exercises it. This assertion therefore
	// guards the FLOOR check, which is load-bearing, rather than the frontier.
	if spans*2 > reads {
		t.Errorf("%d span dispatches over %d reads. The byte frontier is not suppressing "+
			"re-dispatch, so every read issues a range the blockstore then has to dedup -- one "+
			"goroutine per read on a streaming mount", spans, reads)
	}
	if overlaps != 0 {
		t.Errorf("%d span dispatches started behind the previous one's end; a span must extend "+
			"the frontier, not re-cover it. This is what happens when the range is placed at "+
			"the READ's end rather than at the frontier", overlaps)
	}
}

// A RUN THAT STARTS AT A LOWER OFFSET THAN THE LAST ONE MUST STILL GET SPANS.
//
// The span frontier is an absolute byte offset, so if it is not reset when a run ends, a walk
// that works BACKWARD through a file compares each new run against a stale frontier far ahead of
// it, concludes it has already led that far, and dispatches almost nothing. Forward walks hide
// this completely, which is why a revert sweep found the reset untested.
//
// ASSERTED AS A CONTRAST AGAINST THE SAME WALK RUN FORWARD, not against zero. Removing the reset
// takes this fixture from 35 spans to 7 -- a 5x drop, not a wipeout, because the first field
// visited still leads and later fields occasionally reach past the stale frontier. An
// `== 0` assertion passed with the reset removed, which is how this test first shipped toothless.
func TestBackwardWalkGetsAsManySpansAsForward(t *testing.T) {
	const bs = int64(8) << 20
	const rd = int64(128) << 10
	const field = int64(1) << 20
	const fields = 6

	walk := func(backward bool) (spans int, runsEnded int64) {
		p := New(223)
		p.SetGapMax(bs)
		p.SetCoverage(16, 0)
		p.SetEvidence(2.0, bs)
		p.SetSubBlockDispatch(true)
		p.Open()
		prevEnd := int64(-1)
		order := make([]int64, 0, fields)
		for f := int64(0); f < fields; f++ {
			order = append(order, f)
		}
		if backward {
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		for _, f := range order {
			base := f*(3*bs) + bs - field/2
			for off := base; off < base+field; off += rd {
				g := int64(0)
				if prevEnd >= 0 {
					g = off - prevEnd
				}
				for _, d := range p.Observe(off/bs, off, rd, g) {
					if d.SpanLen > 0 {
						spans++
					}
				}
				prevEnd = off + rd
			}
		}
		return spans, p.RunsEnded()
	}

	fwd, fwdRuns := walk(false)
	back, backRuns := walk(true)
	t.Logf("forward: %d spans (%d runs ended) | backward: %d spans (%d runs ended)",
		fwd, fwdRuns, back, backRuns)

	if fwdRuns == 0 || backRuns == 0 {
		t.Fatalf("a walk finished no runs (forward %d, backward %d), so the arm's precondition "+
			"was never met and this fixture cannot see the frontier reset", fwdRuns, backRuns)
	}
	if fwd == 0 {
		t.Fatal("the forward walk got no spans, so there is no baseline to compare against")
	}
	// The two walks read identical bytes in a different order, so they should earn comparable
	// numbers of spans. Half is generous; the defect takes it to a fifth.
	if back*2 < fwd {
		t.Errorf("a walk read backward got %d spans against %d read forward. The span frontier "+
			"is not reset when a run ends, so each new run at a lower offset compares against a "+
			"stale frontier ahead of it and declines to dispatch", back, fwd)
	}
}
