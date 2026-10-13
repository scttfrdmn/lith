// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/prefetch"
)

// ★★ THE WITNESS FOR #407's RANDOM-HISTORY TRIGGER, written BEFORE the trigger was built and
// kept as its guard.
//
// The trigger lets a handle that has collapsed to Random be given a sub-block span at its next
// establishment. The shape that makes that dangerous is a reader that scatters and THEN STREAMS
// a large contiguous stretch -- an index or header read followed by the payload. That handle has
// a Random history, so the trigger fires on exactly the moment it starts wanting a lot.
//
// Part of the protection is arithmetic rather than policy, which is worth stating because it is
// not obvious: establishment requires a BLOCK ADVANCE, so a dense stream that starts at a block
// boundary has consumed a full block by the time it establishes, earns exactly the 2-block floor
// at ratio 2, and the span declines on its own. A stream that starts MID-BLOCK crosses sooner,
// earns less, and does get bounded -- so the protection depends on the stream's phase relative
// to the blocks, which is not something to reason about. Hence this measures it.
//
// ★★★ WHAT THIS WITNESS DOES NOT COVER, AND IT IS THE AXIS THAT MATTERED.
//
// This asserts BYTES and SPAN COUNT. It passed, and the arm was defaulted on, and an external
// bracketed cell then measured an index-then-stream reader **20% slower** with the arm on --
// median 0.798, below both of its brackets in 8 of 8 reps -- with BYTES IDENTICAL and uncovered
// demand misses 62 -> 81. The entire cost was in faults and wall, which is precisely what this
// test does not assert. Right witness, built for the right risk, wrong axis.
//
// AND IT CANNOT BE FIXED BY ADDING AN ASSERTION HERE. The fault counts are logged below, and
// they are too noisy in this fixture to gate on: measured across three identical runs the arm's
// extra faults came out +5/+1/+0 on the short stream and +4/+4/+11 on the long one, so any
// contrast between them already fails. UncoveredMiss fires when a demand read finds its chunk
// neither cached nor in flight, so it races the prefetch goroutine, and a fake server has no
// latency for the race to resolve against. The external cell needed 1 GiB streams and 8
// bracketed reps to see 62 -> 81 cleanly.
//
// So the figures are logged, not asserted, and this comment is the gate: **a green run of this
// test does not clear this arm.** The wall and fault axes are external-only, and the default is
// pinned off in cmd/lith with that measurement as the reason.

// THE ASSERTION IS THAT THE COST DOES NOT SCALE WITH STREAM LENGTH. A bounded first dispatch
// that the window then ramps back from costs a fixed amount; a mechanism that keeps bounding a
// streaming handle costs a share of everything it reads. Those differ in kind, and only the
// second would be disqualifying. Measuring at two stream lengths separates them, which a single
// length cannot -- the mistake that made three earlier fixtures on this issue measure the wrong
// thing.
func TestScatterThenStreamCostDoesNotScaleWithStreamLength(t *testing.T) {
	const objBytes = int64(128) << 20
	const blockSz = int64(8) << 20
	const read = int64(128) << 10
	const ratio = 2.0

	// A stream that starts MID-BLOCK, which is the phase that lets the span fire at all.
	// The scatter stride must EXCEED seqGapMax, which the FUSE layer sets to the block size.
	// A first version used 5 MiB gaps against an 8 MiB block: every "scattered" read was
	// accepted as in-band sequential progress, the handle never collapsed to Random, and the
	// precondition below caught it rather than letting the test score the wrong trigger.
	const scatterStride = int64(13) << 20
	const streamBase = int64(70)<<20 + blockSz/2

	run := func(streamLen int64, subBlock bool) (fetched, spans int64, st prefetch.State, reset int64) {
		raw, srv, rec := newWalkFS(t, objBytes, blockSz, ratio, false, subBlock)
		node, fh := openBig(t, raw)
		h := raw.handleOf(fh)
		gets0 := srv.GetCallCount()
		before := quiesce(srv)
		// Phase 1: scattered reads far enough apart to collapse the handle to Random.
		for i := int64(0); i < 6; i++ {
			readAt(t, raw, node, fh, i*scatterStride+1234, read)
		}
		reset = h.pf.pf.Resets()
		// Phase 2: one long contiguous stream.
		for off := streamBase; off < streamBase+streamLen; off += read {
			readAt(t, raw, node, fh, off, read)
		}
		unc, hit, iss := rec.coverage()
		// ★ THE REQUEST COUNT IS THE THIRD AXIS, and it is where the residual turned out to
		// live. After the lead fix closed the coverage gap, an external bracketed cell still
		// read a median 0.854 on this shape with uncovered back to the `=false` level and bytes
		// identical -- the only remaining difference was ~+41 GETs. Reproduced here at
		// 62 -> 79-81 for identical bytes. 41 extra round trips at a 30 ms first byte is 1.23 s
		// against a ~7.4 s stream = 17%, which brackets the 11-25% of their slow reps.
		//
		// So a witness for this arm has to cover BYTES, COVERAGE and REQUESTS. It covered the
		// first, which is how a 20% regression shipped; then the second; this is the third.
		t.Logf("    stream %d MiB subBlock=%-5v uncovered=%d prefetchHit=%d prefetchIssued=%d GETs=%d",
			streamLen>>20, subBlock, unc, hit, iss, srv.GetCallCount()-gets0)
		return quiesce(srv) - before, h.pf.pf.SpansIssued(), h.pf.state(), reset
	}

	type cell struct {
		streamLen   int64
		offB, onB   int64
		offSp, onSp int64
		state       prefetch.State
		reset       int64
	}
	var cells []cell
	for _, streamLen := range []int64{16 << 20, 48 << 20} {
		offB, offSp, _, _ := run(streamLen, false)
		onB, onSp, st, reset := run(streamLen, true)
		cells = append(cells, cell{streamLen, offB, onB, offSp, onSp, st, reset})
		t.Logf("stream %2d MiB: off %9d B / %d spans | on %9d B / %d spans | %+d B | state %v, resets %d",
			streamLen>>20, offB, offSp, onB, onSp, onB-offB, st, reset)
	}

	// FIXTURE PRECONDITIONS. Without the Random collapse the trigger cannot fire and this
	// measures nothing; without establishment afterwards there is no stream to protect.
	for _, c := range cells {
		if c.reset == 0 {
			t.Fatalf("stream %d MiB: the handle never collapsed to Random, so the trigger under "+
				"test was never armed", c.streamLen>>20)
		}
		if c.state != prefetch.Sequential {
			t.Fatalf("stream %d MiB: the handle ended in %v, not Sequential — the streaming "+
				"phase is not establishing and this fixture is not the shape it claims",
				c.streamLen>>20, c.state)
		}
		if c.offSp != 0 {
			t.Errorf("stream %d MiB: %d spans with the flag off", c.streamLen>>20, c.offSp)
		}
	}

	short, long := cells[0], cells[1]

	// ★ THE ASSERTION: the extra cost must not grow with the stream. The streaming phase
	// tripled; if the mechanism charges a share of what a stream reads, the delta triples too.
	shortDelta, longDelta := short.onB-short.offB, long.onB-long.offB
	t.Logf("extra bytes: %+d B at %d MiB, %+d B at %d MiB", shortDelta, short.streamLen>>20,
		longDelta, long.streamLen>>20)
	if longDelta > shortDelta+blockSz {
		t.Errorf("the extra cost grew from %+d B to %+d B when the stream grew %dx — a bounded "+
			"first dispatch costs a FIXED amount that the window ramps back from, so a cost "+
			"that scales with stream length means the arm keeps bounding a streaming handle. "+
			"That would disqualify the Random-history trigger", shortDelta, longDelta,
			long.streamLen/short.streamLen)
	}
	// ★ AND THE SPAN DISPATCH COUNT IS PINNED, because it is what drives the request count and
	// it is DETERMINISTIC where the GET count is not: measured 16 in three consecutive runs
	// while GETs came out 79, 81, 79.
	//
	// 16 is more than this shape needs. The span phase covers only the first ~8 MB of the
	// stream -- beyond that `earned` passes the two-block floor and the whole-block path takes
	// over -- and across it the lead grows from ~4 MB to ~16 MB, so the frontier runs AWAY from
	// the reader and ~3 dispatches would cover it. The extra ones happen because the frontier
	// is recomputed as `readEnd + earned` on every read, and `earned` grows faster than the
	// reader advances, so the comparison against the previous frontier never short-circuits.
	// Fixing that needs a trigger tied to the REMAINING LEAD rather than to the earned amount,
	// which is a tuning decision I would rather make from a measurement than invent. This
	// assertion holds the line in the meantime.
	if long.onSp > 20 {
		t.Errorf("the streaming phase issued %d span dispatches; each costs about one extra "+
			"GET on the reader's critical path, and an external cell attributed this arm's "+
			"residual ~15%% wall cost on this shape to exactly that. A rise here is a "+
			"regression in request count even when bytes are unchanged", long.onSp)
	}

	// AND THE SPAN COUNT MUST NOT SCALE EITHER, which says the same thing about the mechanism
	// rather than about the bytes, and is the more direct statement of it.
	if long.onSp > short.onSp+2 {
		t.Errorf("spans grew from %d to %d as the stream tripled; once a streaming handle's run "+
			"passes a block's worth of consumption the span must decline, so a growing count "+
			"means it is being re-armed mid-stream", short.onSp, long.onSp)
	}
	// A floor on the comparison: a fixture where the arm never fires at all proves nothing
	// about the risk. If this trips, the stream's phase has drifted so that establishment
	// always happens on a full block and the dangerous case is not being exercised.
	if short.onSp == 0 && long.onSp == 0 {
		t.Skip("the span never fired on a scatter-then-stream handle in this fixture, so the " +
			"risk is not exercised here — see the phase note above before trusting this")
	}
}

// THE FINISHED-RUN ROUTE MUST ACTUALLY COUNT A REAL RUN, asserted through the FUSE path on a
// walk that genuinely de-establishes.
//
// This exists because a revert sweep found a hole: once the Random-history route was added,
// reverting the deEstablish/endRun ordering bug stopped failing anything. The arm still fired
// through the other route, every test stayed green, and `runsEnded` would have been permanently
// zero -- a dead route nobody would notice, which is the shape the ttfbRecorder hook shipped in.
//
// It has to run with the coverage gate ON (i.e. through the FUSE layer, where covMin cannot be
// zero). A first attempt at the prefetch level used covMin=0, where the jumps take the seek
// re-anchor and call endRun DIRECTLY with `established` still true -- so the ordering bug is
// invisible there. The bug only bites when endRun is reached THROUGH deEstablish, which needs a
// scattered landing, which needs the gate.
func TestAWalkThatDeEstablishesCountsItsFinishedRuns(t *testing.T) {
	const objBytes = int64(48) << 20
	const blockSz = int64(4) << 20
	const read = int64(32) << 10
	const preCross = int64(1) << 20
	const field = preCross + 256<<10
	const spacing = int64(8) << 20
	const fields = 5

	raw, _, _ := newWalkFS(t, objBytes, blockSz, 2.0, false, true)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)
	for f := int64(0); f < fields; f++ {
		base := f*spacing + blockSz - preCross
		for off := base; off < base+field; off += read {
			readAt(t, raw, node, fh, off, read)
		}
	}
	ended, resets := h.pf.pf.RunsEnded(), h.pf.pf.Resets()
	t.Logf("%d-field re-establishing walk: runsEnded=%d resets=%d spans=%d",
		fields, ended, resets, h.pf.pf.SpansIssued())

	if resets == 0 {
		t.Fatalf("the walk never collapsed to Random, so it never de-established and this " +
			"fixture cannot exercise the endRun path that goes through deEstablish")
	}
	if ended == 0 {
		t.Error("a walk that de-established " +
			"produced zero finished runs. endRun is not counting them — most likely it reads " +
			"`established` after deEstablish has already cleared it — so the finished-run " +
			"route into subBlockSpan is dead and only the Random-history route works")
	}
}

// ★★ THE FAULT MODEL THAT DID NOT TRANSFER, pinned as an artifact so it cannot come back.
//
// A fixture at 8 MiB field spacing showed this arm halving bytes while TRIPLING uncovered demand
// misses (4-7 -> 12). I published a bytes-for-faults break-even from that figure -- around
// 100 MB/s of bandwidth at a 30 ms first byte -- which implied the arm would be a wall LOSS at
// any rate a real instance reaches. An external wall-clock cell then measured NEUTRAL on both
// real shapes with uncovered misses FLAT in every arm (69 in all three arms on one row,
// 331-333 on the other).
//
// THE REASON IS THE SPACING, and it is a property of the fixture rather than of the arm. The
// default dispatches window+1 blocks; at 8 MiB spacing against a 4 MiB block that reach is
// 12 MiB, which OVERSHOOTS INTO THE NEXT FIELD. So the default's over-fetch was accidentally
// prefetching the next field, and the arm's shorter span was not -- a saving on the current
// field paid for with a fault on the next. A real scattered walk jumps farther than the window,
// by definition of being scattered, so nothing is accidentally covered either way.
//
// This asserts that: inside the default's reach the fault counts differ, beyond it they are
// EQUAL and the byte saving is larger. If a future change makes the arm add faults at wide
// spacing too, that is a real regression rather than this artifact, and this test separates them.
func TestSubBlockAddsNoFaultsOnceFieldsOutrunTheDefaultDispatch(t *testing.T) {
	const blockSz = int64(4) << 20
	const read = int64(32) << 10
	const preCross = int64(1) << 20
	const field = preCross + 256<<10
	const fields = 5
	// The default dispatches window+1 blocks at the 2-block floor, so its reach is 3 blocks.
	reach := 3 * blockSz

	measure := func(spacing int64, sub bool) (bytes int64, uncovered int) {
		objBytes := spacing*(fields+1) + (16 << 20)
		raw, srv, rec := newWalkFS(t, objBytes, blockSz, 2.0, false, sub)
		node, fh := openBig(t, raw)
		before := quiesce(srv)
		for f := int64(0); f < fields; f++ {
			base := f*spacing + blockSz - preCross
			for off := base; off < base+field; off += read {
				readAt(t, raw, node, fh, off, read)
			}
		}
		u, _, _ := rec.coverage()
		return quiesce(srv) - before, u
	}

	type row struct {
		spacing int64
		dB, bB  int64
		dU, bU  int
	}
	var inside, beyond []row
	for _, spacing := range []int64{reach - blockSz, 4 * blockSz, 8 * blockSz} {
		dB, dU := measure(spacing, false)
		bB, bU := measure(spacing, true)
		r := row{spacing, dB, bB, dU, bU}
		t.Logf("spacing %2d MiB (default reach %d MiB): bytes %7.1f -> %7.1f MB (%.2fx) | uncovered %2d -> %2d",
			spacing>>20, reach>>20, float64(dB)/1e6, float64(bB)/1e6,
			float64(bB)/float64(dB), dU, bU)
		if spacing < reach {
			inside = append(inside, r)
		} else {
			beyond = append(beyond, r)
		}
	}

	if len(inside) == 0 || len(beyond) == 0 {
		t.Fatalf("need spacings on both sides of the %d MiB reach; got %d inside and %d beyond",
			reach>>20, len(inside), len(beyond))
	}

	// ★ ASSERTED AS A CONTRAST BETWEEN CELLS, NOT AS AN ABSOLUTE COUNT -- and the first version
	// of this test got that wrong, passing alone and failing twice in three package runs.
	//
	// UncoveredMiss fires when a demand read finds its chunk neither cached nor in flight, so it
	// RACES the prefetch goroutine: under concurrent load a few extra misses appear in any arm.
	// An exact equality on it is a tight bound on a load-sensitive quantity, which is the same
	// mistake as the #313 gate test that asserted the gate HALVES peak pressure and then flaked
	// in the full-tree run. What is load-invariant is the DELTA BETWEEN THE TWO ARMS, compared
	// across the two spacings measured in the same run.
	insideDelta := inside[0].bU - inside[0].dU
	t.Logf("extra faults from the arm: %+d inside the reach (%d MiB spacing)",
		insideDelta, inside[0].spacing>>20)
	if insideDelta <= 0 {
		t.Errorf("at %d MiB spacing (inside the %d MiB reach) the arm added %+d faults; the "+
			"artifact this test exists to explain is gone, so either the default's reach or "+
			"the explanation has changed and the comment above is now wrong",
			inside[0].spacing>>20, reach>>20, insideDelta)
	}
	for _, r := range beyond {
		delta := r.bU - r.dU
		t.Logf("extra faults from the arm: %+d beyond the reach (%d MiB spacing)",
			delta, r.spacing>>20)
		if delta >= insideDelta {
			t.Errorf("at %d MiB spacing (beyond the %d MiB reach) the arm added %+d faults, "+
				"against %+d inside it. Once fields outrun the default dispatch neither arm "+
				"covers the next field, so the arm's extra faults must fall away; a delta as "+
				"large as the inside one is the real fault regression the external cell "+
				"measured as flat", r.spacing>>20, reach>>20, delta, insideDelta)
		}
		if r.bB >= r.dB {
			t.Errorf("at %d MiB spacing the arm fetched %d B against the default's %d B",
				r.spacing>>20, r.bB, r.dB)
		}
	}
}
