// SPDX-License-Identifier: Apache-2.0

package prefetch

import "testing"

// #407 arm B: a dispatch may be SHORTER THAN ONE BLOCK when that is all the handle's evidence
// earns. These tests drive the real Observe path rather than calling subBlockSpan, because a
// policy function tested in isolation passes whether or not anything calls it -- the shape the
// ttfbRecorder hook shipped in, declared and never called, through a clean build and a green
// test run.

const (
	testBlock = int64(8) << 20 // the default --block-size
	testRatio = 2.0            // the in-region default since #407's ratio sweep
)

// walkArms runs a field-at-a-time walk and returns the bytes the detector asked for.
//
// THE COVERAGE GATE IS OFF HERE, DELIBERATELY, and this is the one thing to understand about
// these fixtures. With it on (the default) a SPARSE walk's landings have trailing coverage far
// under covMin, so #221 sends them to Random and nothing is dispatched at all -- arm B has
// nothing to bound and the bytes are already zero. The shapes arm B is for are the walks that
// keep RE-ESTABLISHING, where the handle is Sequential at the landing and the floor hands it
// two whole blocks. Turning the gate off isolates that branch; it does not invent it. An
// external trace of the real GRIB shape counts 12 such sequential transitions against 67
// random ones, so both branches are live on one handle, and the MAGNITUDE on a real shape is
// therefore not derivable here -- only the decision is.
func walkArms(t *testing.T, perRun, subBlock bool, fieldBytes, readBytes, spacing int64, fields int) (bytes, spans int64) {
	t.Helper()
	p := New(64)
	// THE FUSE LAYER SETS THIS AND A FIXTURE THAT FORGETS IT MEASURES NOTHING. Without a gap
	// bound, seqGapMax is MaxInt64, so a jump three blocks ahead is accepted as in-band
	// sequential progress -- the handle never reaches the seek path, never ends a run, and
	// arm B's precondition is never met. The first version of this fixture omitted it and
	// produced four identical arms, which looks exactly like "the mechanism does nothing".
	p.SetGapMax(testBlock)
	p.SetCoverage(16, 0) // see the comment above
	p.SetEvidence(testRatio, testBlock)
	p.SetPerRunEvidence(perRun)
	p.SetSubBlockDispatch(subBlock)

	add := func(ds []Dispatch) {
		for _, d := range ds {
			if d.Span > 0 {
				bytes += d.Span
				spans++
				continue
			}
			bytes += testBlock
		}
	}
	add(p.Open())

	prevEnd := int64(-1)
	for f := 0; f < fields; f++ {
		// ★ EACH FIELD STRADDLES A BLOCK BOUNDARY, AND IT HAS TO.
		//
		// A run entirely inside one block produces no block advance at all: every read after
		// the first has d==0 and returns immediately, so nothing clears `pending`, and the
		// jump after the next one takes the second-unexplained-jump path to Random instead of
		// the re-anchor. The handle then alternates re-anchor / Random and arm B's branch is
		// reached once in the whole walk. The first version of this fixture did exactly that
		// and reported four identical arms -- for the second time on this issue, a fixture
		// that could not reach the mechanism looked like a mechanism that does nothing.
		//
		// ★ AND THE FRACTION IS CORROBORATED BY THE REAL TRACE. A run of R bytes at an
		// arbitrary offset crosses a block boundary with probability R/blockSize, so at the
		// measured ~1 MB fields over 8 MiB blocks about 1 in 8 landings keeps the handle
		// Sequential. An external trace of the real GRIB shape counts 12 sequential
		// transitions against 67 random ones -- 12/79, which is 1/6.6. That is the same
		// quantity, and it is why this arm bounds the ~24 prefetched chunks that shape
		// reports rather than all of its traffic.
		base := int64(f)*spacing + testBlock - fieldBytes/2
		for off := base; off < base+fieldBytes; off += readBytes {
			gap := int64(0)
			if prevEnd >= 0 {
				gap = off - prevEnd
			}
			add(p.Observe(off/testBlock, off, readBytes, gap))
			prevEnd = off + readBytes
		}
	}
	return bytes, spans
}

// ★★ ARM B BOUNDS A WALK ON ITS OWN, AND --readahead-evidence-per-run IS ORTHOGONAL TO IT.
//
// THIS USED TO BE A 2x2 AND THE MEASUREMENT COLLAPSED IT. The arm originally read
// evidenceBasis(), so it needed per-run evidence to have a run-sized basis, and the four cells
// were the point. An external 2x2 on v1.14.0 then found all four arms identical on every real
// shape, because the arm's TRIGGER ("has finished an established run") described the fixture
// below and not the tools: a GRIB handle establishes once, in its last 12 of 90 reads, and
// finishes no runs at all.
//
// Fixing the trigger (a Random history also qualifies) forced the basis question too: at such a
// handle's establishment, LIFETIME consumption is evidence about scattered reading rather than
// contiguous appetite -- GRIB's 11.8 MB earns 23.6 MB, above the floor, so a lifetime basis
// declines on the exact shape this exists for. So the basis at this site is always the current
// run, and the second flag drops out of the mechanism entirely.
//
// What remains to assert is therefore not a 2x2 but two things: the arm works alone, and the
// other flag neither helps nor hinders it. The second half is what stops the two from being
// quietly re-coupled.
func TestSubBlockBoundsAWalkAloneAndIsIndependentOfPerRun(t *testing.T) {
	const field = int64(1) << 20
	const read = int64(128) << 10
	const spacing = 3 * testBlock
	const fields = 8

	type arm struct {
		perRun, subBlock bool
		name             string
	}
	arms := []arm{
		{false, false, "A off, B off (shipped default)"},
		{true, false, "A on,  B off"},
		{false, true, "A off, B on"},
		{true, true, "A on,  B on"},
	}
	got, spansBy := map[string]int64{}, map[string]int64{}
	for _, a := range arms {
		b, sp := walkArms(t, a.perRun, a.subBlock, field, read, spacing, fields)
		got[a.name], spansBy[a.name] = b, sp
		t.Logf("%-30s dispatched %9d B (%6.2f MiB), %d spans", a.name, b,
			float64(b)/float64(1<<20), sp)
	}

	base := got["A off, B off (shipped default)"]
	if base == 0 {
		t.Fatal("the baseline dispatched nothing, so this fixture measures no branch at all")
	}

	// ARM A ALONE REMAINS EXACTLY INERT, which is the external null reproduced locally:
	// windowCap floors at initialWindow blocks, so the arm reduces an earned window the floor
	// hands straight back.
	if got["A on,  B off"] != base {
		t.Errorf("arm A alone dispatched %d B against the baseline's %d B; it is inert because "+
			"windowCap floors at %d blocks, and a difference means that explanation is wrong",
			got["A on,  B off"], base, initialWindow)
	}

	// ARM B ALONE DOES THE WORK.
	bAlone := got["A off, B on"]
	if bAlone >= base {
		t.Fatalf("arm B alone dispatched %d B against the baseline's %d B — it must bound a "+
			"walk without needing the other flag, which is the whole point of reading the "+
			"current run at this site", bAlone, base)
	}
	if spansBy["A off, B on"] == 0 {
		t.Fatal("arm B alone issued no spans, so the reduction came from somewhere else")
	}
	t.Logf("arm B alone: %d B vs %d B = %.2fx less dispatch", bAlone, base,
		float64(base)/float64(bAlone))

	// ★ AND THE INDEPENDENCE, asserted as EQUALITY. If adding per-run evidence changes the
	// result, the span is reading something other than the current run and the two mechanisms
	// have been re-coupled -- which is what the v1.14.0 null cost four cells to discover.
	if both := got["A on,  B on"]; both != bAlone {
		t.Errorf("arm B gave %d B alone and %d B with per-run evidence added; the span's basis "+
			"is the current run at this site, so the two flags must be independent",
			bAlone, both)
	}
	if spansBy["A on,  B on"] != spansBy["A off, B on"] {
		t.Errorf("span count changed with per-run evidence: %d vs %d",
			spansBy["A on,  B on"], spansBy["A off, B on"])
	}

	// ★★ AND THE PROPERTY THAT REPLACED THE OLD DECAY ASSERTION. When the basis was lifetime
	// consumption, arm B alone switched ITSELF off as a handle accumulated -- its span count
	// pinned at 13 whether the walk was 8 fields or 64. With a per-run basis it must do the
	// opposite: one span per landing, indefinitely. That reversal is the mechanical signature
	// of the basis change, and asserting it stops a future edit from silently restoring the
	// lifetime basis (which would pass every other assertion in this file).
	const longFields = 64
	longBytes, longSpans := walkArms(t, false, true, field, read, spacing, longFields)
	longBase, _ := walkArms(t, false, false, field, read, spacing, longFields)
	t.Logf("at %d fields: baseline %d B, arm B alone %d B / %d spans (%.2fx)", longFields,
		longBase, longBytes, longSpans, float64(longBase)/float64(longBytes))
	if longSpans <= spansBy["A off, B on"] {
		t.Errorf("arm B issued %d spans over %d fields against %d over %d — with the basis "+
			"reset every run it must issue one per landing indefinitely, and a count that "+
			"stops growing means the basis has reverted to lifetime consumption",
			longSpans, longFields, spansBy["A off, B on"], fields)
	}
	if r, rLong := float64(base)/float64(bAlone), float64(longBase)/float64(longBytes); rLong < r {
		t.Errorf("the advantage shrank as the walk grew (%.2fx over %d fields against %.2fx "+
			"over %d); with every run bounded it must hold or grow", rLong, longFields, r, fields)
	}

	// The span size is the arithmetic, pinned so a constant change must be re-derived.
	wantSpan := int64(float64(field) * testRatio)
	floorBytes := initialWindow * testBlock
	t.Logf("one run of %d B earns %d B at ratio %g, against the %d B floor (%.1fx)",
		field, wantSpan, testRatio, floorBytes, float64(floorBytes)/float64(wantSpan))
	if wantSpan >= floorBytes {
		t.Fatalf("the fixture's field earns %d B, at or above the %d B floor — it cannot "+
			"exercise this arm and every comparison above is meaningless", wantSpan, floorBytes)
	}
}

// ★ THE WITNESS THIS ARM MUST NOT BREAK, and the reason the precondition is a run COUNT and
// not a consumption threshold.
//
// A genuine sequential copy consumes very little in its opening reads -- for the ordinary
// reason that it has barely started -- so a bare "has this handle earned a block?" test would
// bound a streaming reader's cold ramp. #233 establishes that a cold sequential read should
// fetch MORE rather than less, and #231 refuted three separate attempts to make it fetch
// sooner. Today windowCap's 2-block floor is what protects that reader. This arm removes the
// floor, so it has to carry the protection itself.
//
// It carries it by requiring a FINISHED ESTABLISHED RUN, which a copy never has.
func TestSubBlockCannotReachASequentialCopy(t *testing.T) {
	run := func(subBlock bool) (bytes, spans, runsEnded, resets int64) {
		p := New(64)
		p.SetCoverage(16, coverageMinForTest)
		p.SetEvidence(testRatio, testBlock)
		p.SetSubBlockDispatch(subBlock)
		const read = int64(128) << 10
		for i := int64(0); i < 400; i++ {
			off := i * read
			for _, d := range p.Observe(off/testBlock, off, read, 0) {
				if d.Span > 0 {
					bytes += d.Span
					spans++
					continue
				}
				bytes += testBlock
			}
		}
		return bytes, spans, p.RunsEnded(), p.Resets()
	}

	offB, offSpans, _, _ := run(false)
	onB, onSpans, ended, reset := run(true)
	t.Logf("sequential copy, 400 contiguous reads: arm B off %d B / %d spans, on %d B / %d spans",
		offB, offSpans, onB, onSpans)
	t.Logf("runs ended on the copy: %d", ended)

	// BYTE-IDENTICAL, which is the only acceptable result for a copy.
	if onB != offB {
		t.Errorf("arm B changed a sequential copy's dispatch: %d B with it on against %d B "+
			"with it off. A copy must be untouched -- this is the #231/#233 witness", onB, offB)
	}
	if onSpans != 0 {
		t.Errorf("a sequential copy was given %d sub-block spans; it must get none", onSpans)
	}

	// ★ AND THE REASON, ASSERTED SEPARATELY, because it is the part that was wrong first.
	//
	// endRun is called by deEstablish, and the #221 coverage gate de-establishes on the FIRST
	// READS OF EVERY HANDLE -- coverageOK needs covReads trailing spans before it can judge,
	// so reads 1 and 2 of a plain copy take the coverage-held branch. Counting those as
	// finished runs gave a copy runsEnded=2 before it had established anything, and arm B then
	// bounded its opening ramp from a consumption that was small only because the handle had
	// just started. The precondition would have introduced the regression it exists to prevent.
	//
	// So: a copy's run count must be ZERO, not merely small. If this fails, the two assertions
	// above are passing for an unrelated reason (the copy's lifetime consumption outgrew the
	// floor before any span could be issued) and the protection is not actually in place.
	if ended != 0 {
		t.Errorf("a sequential copy ended %d runs, want 0. endRun is counting the "+
			"pre-establishment coverage warm-up, so the precondition no longer excludes a "+
			"cold streaming reader and the bytes above are equal by luck of ordering", ended)
	}
	// ★ AND THE SECOND ROUTE, which the trigger gained after an external null showed the first
	// one almost never fires on real tools: a Random history also admits a handle. A copy must
	// satisfy NEITHER. It does not, for a reason worth keeping explicit -- the #221 coverage
	// gate holds a copy's first reads provisional as COLD, and its Sequential-to-Cold demotion
	// never passes through Random. If that ever changes, every streaming reader becomes a
	// candidate for a bounded dispatch and #233 is back.
	if reset != 0 {
		t.Errorf("a sequential copy collapsed to Random %d times, want 0. The Random-history "+
			"route into subBlockSpan would then admit a cold streaming reader, which is the "+
			"#233 regression this precondition exists to prevent", reset)
	}
}

// coverageMinForTest is the shipped coverage threshold. Spelled out here rather than imported
// from internal/fuse (which would be an import cycle) so the copy witness above runs against
// the gate a real mount has, not with it disabled.
const coverageMinForTest = 0.5

// The precondition, tested at the decision rather than through a walk: with everything else
// in force, a handle that has not finished an established run gets no span.
func TestSubBlockRequiresAFinishedRun(t *testing.T) {
	newP := func() *Prefetcher {
		p := New(64)
		p.SetEvidence(testRatio, testBlock)
		p.SetSubBlockDispatch(true)
		p.established = true // the other precondition, so this test varies one thing
		p.consumed, p.runConsumed = 1<<20, 1<<20
		return p
	}

	p := newP()
	if ds, ok := p.subBlockSpan(7); ok {
		t.Errorf("a handle with runsEnded=0 was given %v; the cold-ramp protection is gone", ds)
	}

	// ★ EITHER ROUTE ADMITS, and both are tested, because the second one is the whole reason
	// this arm is not inert: an external 2x2 found the arm dead on every real shape because
	// only the finished-run route existed, and the real tools finish ~no runs.
	pr := newP()
	pr.resetRandom = 1
	if dsr, okr := pr.subBlockSpan(7); !okr {
		t.Error("a handle with a Random history was refused a span; that route is what makes " +
			"this arm reach the measured shapes at all")
	} else if len(dsr) != 1 || dsr[0].Span <= 0 {
		t.Errorf("Random-history route produced %v, want one span dispatch", dsr)
	}

	p = newP()
	p.runsEnded = 1
	ds, ok := p.subBlockSpan(7)
	if !ok {
		t.Fatal("a handle that HAS finished a run was refused a span, so the precondition " +
			"rejects the case it is supposed to admit and arm B can never fire")
	}
	if len(ds) != 1 || ds[0].Span <= 0 {
		t.Fatalf("expected one span dispatch, got %v", ds)
	}
	if want := int64(float64(1<<20) * testRatio); ds[0].Span != want {
		t.Errorf("span is %d B, want %d B (one run's %d B at ratio %g)", ds[0].Span, want,
			int64(1)<<20, testRatio)
	}
	if ds[0].Block != 7 {
		t.Errorf("span dispatch reports block %d, want 7 — a dispatch must say where it is",
			ds[0].Block)
	}
	// A span and a ReadLen are different facts and must not be conflated: the FUSE layer
	// applies a small-read threshold to ReadLen and none to Span.
	if ds[0].ReadLen != 0 {
		t.Errorf("span dispatch also set ReadLen=%d; the FUSE layer would then judge an "+
			"already-earned amount against the small-read threshold", ds[0].ReadLen)
	}
}

// Off by default at the detector, independently of the flag plumbing: a Prefetcher nobody
// configures must behave exactly as before.
func TestSubBlockIsOffUntilSet(t *testing.T) {
	p := New(64)
	p.SetEvidence(testRatio, testBlock)
	p.established, p.runsEnded = true, 3
	p.consumed, p.runConsumed = 1<<20, 1<<20
	if ds, ok := p.subBlockSpan(1); ok {
		t.Errorf("sub-block dispatch acted without SetSubBlockDispatch: %v", ds)
	}
}

// With the evidence gate itself off there is no earned amount to bound by, so the arm must not
// invent one -- the same rule evidenceRatio <= 0 already follows in windowCap.
func TestSubBlockNeedsTheEvidenceGate(t *testing.T) {
	p := New(64)
	p.SetSubBlockDispatch(true)
	p.established, p.runsEnded = true, 3
	p.consumed, p.runConsumed = 1<<20, 1<<20
	if ds, ok := p.subBlockSpan(1); ok {
		t.Errorf("sub-block dispatch acted with the evidence gate off: %v", ds)
	}
}
