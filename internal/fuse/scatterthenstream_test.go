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
		raw, srv, _ := newWalkFS(t, objBytes, blockSz, ratio, false, subBlock)
		node, fh := openBig(t, raw)
		h := raw.handleOf(fh)
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
