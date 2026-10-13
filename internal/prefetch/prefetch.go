// SPDX-License-Identifier: Apache-2.0

// Package prefetch implements lith's per-open-file-handle access-pattern
// detector and readahead planner. The planner keeps a dispatch frontier that
// leads the demand cursor: blocks are dispatched on open and on every window
// advance, never reactively on the miss that reveals a gap. See the pinned
// Design issue, §4.3, and issues #38 and #49.
//
// The detector is tolerant of out-of-order reads: the kernel issues a single
// file handle's readahead concurrently, so reads can arrive at the FUSE layer
// out of order even for a strictly sequential file. A read within the reorder
// band [cursor-window, frontier+window] is treated as sequential progress; only
// a genuine out-of-band jump is a seek, which halves the window (floor 2)
// rather than collapsing it — the full re-ramp was the #49 lone-survivor tail.
package prefetch

import (
	"math"
	"sort"
)

// State is the detected access pattern for a file handle.
type State int

const (
	// Cold is the initial state, before a pattern is established.
	Cold State = iota
	// Sequential means consecutive (or in-band reordered) blocks; readahead
	// grows geometrically.
	Sequential
	// Strided means a constant non-unit block delta seen at least twice.
	Strided
	// Random means no detectable pattern; nothing is prefetched.
	Random
)

func (s State) String() string {
	switch s {
	case Cold:
		return "cold"
	case Sequential:
		return "sequential"
	case Strided:
		return "strided"
	default:
		return "random"
	}
}

// initialWindow is the number of blocks dispatched at open and after a seek.
const initialWindow = 2

// Prefetcher tracks one file handle's read pattern and returns the block
// indices that should be prefetched. It maintains a dispatch frontier (the next
// block index not yet dispatched) so the readahead window stays ahead of the
// demand cursor.
//
//	cold -> sequential (unit step or in-band reorder)
//	     -> strided    (constant non-unit delta observed twice)
//	     -> random     (two out-of-band seeks with no progress between)
//
// Sequential grows its window geometrically from 2 up to maxReadahead; a single
// out-of-band seek halves the window and re-anchors; strided dispatches the
// single next predicted block.
type Prefetcher struct {
	maxReadahead int64

	haveLast  bool
	lastBlock int64
	lastDelta int64
	// deltaRun is how many CONSECUTIVE reads have now repeated lastDelta. A stride is
	// declared at strideRun repeats, not at one (#222).
	deltaRun  int
	state     State
	window    int64
	cursor    int64 // highest block demanded
	frontier  int64 // next block index not yet dispatched
	pending   bool  // an unexplained jump is outstanding (one more seeks -> random)
	seqGapMax int64 // largest byte gap that still counts as sequential (#210/M16 1b); MaxInt64 = off

	// Coverage gate (#221): a stream's reads *tile* — over a trailing window they
	// cover most of the span between them — while a scattered walk (a metadata
	// traversal, a GRIB .idx field sweep, a COG overview) leaves most of the span
	// untouched, however sequential its block deltas look. When coverage over the
	// last covWindow reads falls below covMin, the handle is punctate and is forced
	// Random so the seek path stops re-anchoring Sequential and prefetching two
	// blocks at every landing (which refilled whole sub-64-MiB objects, #220).
	// covMin<=0 disables the gate (behaviour-preserving default).
	covWindow int
	covMin    float64
	covSpans  []covSpan
	// holeDemanded, when set, is asked whether a hole between two reads holds bytes
	// already demanded through lith (#316). Nil disables the discount.
	holeDemanded func(off, end int64) bool // ring of the last covWindow reads (byte ranges)

	// established (#229) is the single signal all three broad-fetch entry points
	// gate on: the open-time ramp, the readahead-window growth, and open-time
	// parts-fetch (the last in the FUSE layer, via Established()). It is true once
	// the coverage signal has confirmed the access tiles — never before, so a
	// handle whose pattern is not yet known is served precise (byte-exact /
	// single-chunk) and commits no broad fetch. It reverts to false when coverage
	// collapses (a stream that becomes a walk), inheriting the #221 transition.
	established bool

	// Evidence gate (#256): the window a handle may commit is bounded by what it
	// has actually consumed, so the size of the bet is proportional to the demand
	// demonstrated for it. The detector advances in block space, so sub-block reads
	// are d==0 no-ops and a handle establishes at its first *block crossing* — one
	// block (8 MiB) of contiguous evidence. Without this gate that buys the full
	// NIC-derived window, which at 50 Gbps is ~223 blocks / ~1.8 GB: a 223x
	// overcommit on the evidence presented. On a
	// scattered-but-locally-contiguous hyperslab (GCHP/HEMCO: tile a slab, jump to
	// the next variable) the bet is re-placed at every re-establishment and ~86%
	// of it is never read. Measured there: five flag settings move the *volume* of
	// bad prefetch almost linearly and the hit rate not at all (16.1% -> 13.3%),
	// so precision is a property of the decision, not of the window size.
	//
	// evidenceRatio <= 0 disables the gate (behaviour-preserving default).
	// `consumed` is cumulative bytes read, the simplest monotone measure of
	// demonstrated demand; a re-reader inflates it without demonstrating forward
	// demand, which is acceptable for a bounded, opt-in experiment.
	evidenceRatio float64
	evidenceBlock int64 // block size in bytes, to convert the byte bound to blocks
	consumed      int64 // cumulative bytes this handle has read
	// runConsumed is bytes read since this RUN began, where a run ends when the handle stops
	// making contiguous progress -- a seek out of an established pattern, a scattered
	// landing, or a de-establishment (#407).
	//
	// WHY IT EXISTS. `consumed` is cumulative over the handle's whole life and is never
	// reset, so windowCap authorizes a window from LIFETIME consumption. On a
	// scattered-but-locally-sequential walk -- a GRIB .idx field sweep, an HDF5 chunk walk,
	// a FITS cutout's row bands -- every run is 0.5-2 MB, and by the third one the handle has
	// earned a window no single run can justify, then re-places that bet at every
	// re-establishment. An external sweep measured the consequence: 48 chunks prefetched and
	// 2 used on GRIB, 71/11 on HEMCO, 47/13 on FITS.
	//
	// The arithmetic reproduces it exactly: 91 reads of 128 KiB is 11.9 MB consumed, which at
	// ratio 4 over an 8 MiB block earns 5.69 -> 5 blocks, and the measured peak window was 5.
	runConsumed int64
	// perRunEvidence makes windowCap read runConsumed instead of consumed. OFF by default:
	// this is the first arm of a 2x2, and it is INERT ON ITS OWN at the current default ratio
	// -- see subBlockSpan, which is the other arm and the reason.
	perRunEvidence bool
	// subBlock lets a dispatch be SHORTER THAN ONE BLOCK when that is all the handle's
	// evidence earns -- the second arm of #407's 2x2. OFF by default. See subBlockSpan.
	subBlock bool
	// runsEnded counts contiguous runs this handle has finished: a seek out of an established
	// pattern, a scattered landing, or a de-establishment. It is the EVIDENCE THAT A HANDLE
	// TAKES SHORT RUNS AT ALL, and subBlockSpan requires it to be non-zero.
	//
	// Without that requirement sub-block dispatch would fire on every handle's FIRST reads,
	// where consumption is small for the ordinary reason that the handle has barely started --
	// and #233 establishes that a cold SEQUENTIAL read should fetch MORE, not less, with three
	// ways of making it fetch sooner refuted in #231. A genuine sequential copy never ends a
	// run, so this is zero for it forever and the arm cannot reach it. That is the same witness
	// perRunEvidence is built on, used here as a precondition rather than a basis.
	runsEnded int64
	// spanTo is the byte offset the furthest issued span reaches. It is the span path's
	// frontier, in BYTES, exactly as `frontier` is the whole-block path's frontier in blocks --
	// and it exists for the same reason: so a dispatch is issued once and the lead is
	// maintained as the reader advances, rather than once per establishment.
	spanTo       int64
	spansIssued  int64 // dispatches bounded to a sub-block span
	evidenceHeld int64 // times the gate held the window below maxReadahead
	evidenceCut  int64 // cumulative blocks withheld by those holds

	// deEstablished (#256) counts losses of an establishment that existed — the
	// oscillation establish -> commit -> jump -> de-establish. It is deliberately
	// NOT resetRandom, which counts collapses to the Random *state* and is 20-30x
	// larger: on the reporting workload resetRandom is 307-332 per run while
	// deEstablished is 10-17. Conflating the two is what made a re-establishment
	// cap look worth building; it was measured inert because the oscillation it
	// gated happens ~3-5% as often as the signal cited for it.
	//
	// Kept as a diagnostic because it separates the mounts in the *opposite*
	// direction to the obvious guess: the healthy mount (80% prefetch hit rate)
	// de-establishes 51 times, 45% of its Random collapses, while the pathological
	// one de-establishes 10, 3% of its collapses. Frequent re-anchoring is a sign
	// of a reader whose prefetch works, not one whose prefetch is wasted.
	deEstablished int64

	// Diagnostics (#49). Single-threaded via pfWrapper.
	halvings    int64 // window halvings on a seek from an established pattern
	resetRandom int64 // collapses to Random (a second seek with no progress)
	peakWindow  int64 // largest window ever reached
	lowCoverage int64 // reads forced Random by the coverage gate on a SEEK landing (#221)
	// coverageHeld counts reads that made CONTIGUOUS progress and were still denied a
	// window because coverage had not confirmed tiling (#229's provisional state).
	//
	// Distinct from lowCoverage, and the distinction is diagnostic. lowCoverage fires on the
	// seek path: a scattered landing, which is the gate working as designed. This fires on
	// the contiguous path: a handle advancing in order, refused a window anyway. That pairing
	// -- contiguous progress with low coverage -- is the signature of #316, where concurrent
	// readers of one object have their siblings' reads absorbed by the shared page cache and
	// each handle's own stream looks punctate. Nothing counted it: deEstablish() only counts
	// an establishment that existed, and these handles are rejected while still provisional,
	// so the purest #316 cell incremented ZERO counters (reported from a 48-rank GCHP run).
	coverageHeld int64
}

// covSpan is one read's byte range [off, end).
type covSpan struct{ off, end int64 }

// Halvings reports how many times a seek halved the window.
func (p *Prefetcher) Halvings() int64 { return p.halvings }

// Resets reports how many times the detector collapsed to Random.
func (p *Prefetcher) Resets() int64 { return p.resetRandom }

// Window reports the readahead window currently in force, in blocks. Exposed so a
// trace can record the window at each decision, which is what lets an offline
// replay be validated for fidelity before its verdict on a new policy is trusted
// (#262).
func (p *Prefetcher) Window() int64 { return p.window }

// PeakWindow reports the largest readahead window this handle ever reached.
func (p *Prefetcher) PeakWindow() int64 { return p.peakWindow }

// New returns a Prefetcher with the given max readahead window in blocks
// (values < 2 are raised to 2). The byte-gap gate is off by default (seqGapMax =
// MaxInt64); call SetGapMax to enable it.
func New(maxReadahead int64) *Prefetcher {
	if maxReadahead < initialWindow {
		maxReadahead = initialWindow
	}
	return &Prefetcher{maxReadahead: maxReadahead, state: Cold, seqGapMax: math.MaxInt64}
}

// SetCoverage enables the #221 coverage gate: over a trailing window of `window`
// reads, Sequential requires that the union of the reads' byte ranges covers at
// least `minRatio` of the span they touch. window<=1 or minRatio<=0 disables it.
// The characterization (#221) put streams at coverage >= 0.89 (W=16) and every
// scattered walk (netcdf4 0.002, GRIB 0.025, COG 0.069) at <= 0.07, so a wide
// window is required — a small one lets field-internal reads tile locally — and
// 0.5 sits in the middle of that order-of-magnitude margin.
func (p *Prefetcher) SetCoverage(window int, minRatio float64) {
	if window <= 1 || minRatio <= 0 {
		window, minRatio = 0, 0
	}
	p.covWindow = window
	p.covMin = minRatio
	p.covSpans = nil
}

// LowCoverage reports how many reads the coverage gate forced Random (#221).
func (p *Prefetcher) LowCoverage() int64 { return p.lowCoverage }

// CoverageHeld reports how many contiguous reads were denied a window because coverage had
// not confirmed tiling (#316). See the field comment for why this is not LowCoverage.
func (p *Prefetcher) CoverageHeld() int64 { return p.coverageHeld }

// SetEvidence enables the #256 evidence gate: a committed readahead window may
// not exceed `ratio` times the bytes this handle has actually read. ratio <= 0 or
// blockSize <= 0 disables it. A sequential copy consumes fast and so earns the
// full window within a few reads (preserving #56, which is why establishment
// jumps straight to the full window); a reader that tiles a slab and jumps never
// consumes enough to earn it.
func (p *Prefetcher) SetEvidence(ratio float64, blockSize int64) {
	if ratio <= 0 || blockSize <= 0 {
		ratio, blockSize = 0, 0
	}
	p.evidenceRatio = ratio
	p.evidenceBlock = blockSize
}

// SetPerRunEvidence selects which consumption figure the evidence gate reads: the handle's
// LIFETIME total (false, today's behaviour) or the CURRENT RUN's (true, #407). See the
// runConsumed field for why the distinction matters and what it cannot fix on its own.
func (p *Prefetcher) SetPerRunEvidence(on bool) { p.perRunEvidence = on }

// SetSubBlockDispatch allows a dispatch shorter than one block when that is all the handle's
// evidence earns (#407 arm B). Off by default; see subBlockSpan.
func (p *Prefetcher) SetSubBlockDispatch(on bool) { p.subBlock = on }

// SpansIssued is how many dispatches this handle bounded to a sub-block span rather than
// whole blocks. Zero on every handle that is not a short-run walk, by construction.
func (p *Prefetcher) SpansIssued() int64 { return p.spansIssued }

// RunsEnded is how many contiguous runs this handle has finished. Zero for a sequential copy.
func (p *Prefetcher) RunsEnded() int64 { return p.runsEnded }

// EvidenceHeld reports how many times the evidence gate held a window below the
// configured maximum (#256). Zero when the gate is disabled.
func (p *Prefetcher) EvidenceHeld() int64 { return p.evidenceHeld }

// EvidenceWithheld reports the cumulative blocks the gate withheld across those
// holds (#256) — the difference between the window the handle would have been
// given and the one its consumption earned. Nothing in the metrics otherwise
// distinguishes a window the gate refused from one the detector never wanted.
func (p *Prefetcher) EvidenceWithheld() int64 { return p.evidenceCut }

// DeEstablished reports how many times this handle lost an establishment it had
// (#256). See the field comment: this is not resetRandom, and the difference
// between them is load-bearing.
func (p *Prefetcher) DeEstablished() int64 { return p.deEstablished }

// deEstablish drops establishment, counting the transition only when there was an
// establishment to lose, so the count measures oscillation rather than the number
// of scattered reads.
func (p *Prefetcher) deEstablish() {
	if p.established {
		p.deEstablished++
	}
	// END THE RUN BEFORE DROPPING ESTABLISHMENT, because endRun reads `established` to decide
	// whether there was a run to end at all (#407 arm B). With the clear first, it always saw
	// false, so no run was ever counted and arm B's precondition could never be met -- the arm
	// was inert on every shape, through a green test run of its own decision-level tests. Found
	// by instrumenting a walk fixture that re-established four times and reported runsEnded=0.
	p.endRun()
	p.established = false
}

// endRun marks the current contiguous run as over, so per-run evidence starts again from zero
// (#407). Called from deEstablish and from the seek re-anchor -- which keeps the handle
// Sequential and therefore does NOT de-establish, and is exactly the branch a GRIB field sweep
// takes between fields.
//
// A genuine sequential copy never reaches either, so its run is the whole file and this is a
// no-op for it. That is the witness #231 refuted three cold-start fixes against, and the reason
// the reset is placed at run boundaries rather than on a timer.
func (p *Prefetcher) endRun() {
	// ★ COUNT ONLY A RUN THERE ACTUALLY WAS ONE OF, which is narrower than "endRun was called"
	// and the distinction is load-bearing for arm B.
	//
	// deEstablish calls endRun, and the #221 coverage gate de-establishes on the first reads of
	// EVERY handle -- coverageOK needs covReads=3 trailing spans before it can judge, so reads
	// 1 and 2 of a plain sequential copy take the coverage-held branch. Incrementing on every
	// call therefore gave a copy runsEnded=2 before it had established anything, which would
	// let subBlockSpan bound its opening ramp from a consumption that is small only because the
	// handle has barely started. That is the #233 regression the precondition exists to prevent,
	// and the precondition would have introduced it.
	//
	// A run requires an establishment to end and bytes to have been read in it. `established`
	// is false through the warm-up above, so a copy's pre-establishment reads are not runs.
	if p.established && p.runConsumed > 0 {
		p.runsEnded++
	}
	p.runConsumed = 0
	// The span frontier belongs to the run, like runConsumed: a new run starts with no lead,
	// and leaving it set would make the next run's first span look already-covered and dispatch
	// nothing at all.
	p.spanTo = 0
}

// windowCap is the largest window, in blocks, this handle's demonstrated
// consumption justifies. It is maxReadahead when the gate is off, and never
// below initialWindow — a handle that has proved contiguous progress always gets
// the minimum dispatch, so the gate narrows bets rather than disabling readahead.
func (p *Prefetcher) windowCap() int64 {
	if p.evidenceRatio <= 0 || p.evidenceBlock <= 0 {
		return p.maxReadahead
	}
	earned := int64(float64(p.evidenceBasis())*p.evidenceRatio) / p.evidenceBlock
	if earned < initialWindow {
		earned = initialWindow
	}
	if earned >= p.maxReadahead {
		return p.maxReadahead
	}
	p.evidenceHeld++
	p.evidenceCut += p.maxReadahead - earned
	return earned
}

// evidenceBasis is the consumption figure the WINDOW gate reads: the handle's lifetime total,
// or the current run's when per-run evidence is on (#407 arm A).
//
// subBlockSpan deliberately does NOT use this -- it always reads the current run, because it
// fires on handles whose lifetime consumption is evidence about scattered reading rather than
// contiguous appetite. The two are separate on purpose; see subBlockSpan's basis note.
func (p *Prefetcher) evidenceBasis() int64 {
	if p.perRunEvidence {
		return p.runConsumed
	}
	return p.consumed
}

// subBlockSpan is the second arm of #407: a dispatch SHORTER THAN ONE BLOCK, for a handle whose
// evidence does not earn even the minimum whole-block window.
//
// ★ WHY THE FIRST ARM IS INERT WITHOUT THIS ONE, which is the whole reason there are two.
// windowCap is denominated in BLOCKS and floors at initialWindow, so the smallest window any
// established handle can be given is 2 blocks = 16 MiB at the default 8 MiB block. A GRIB .idx
// field sweep reads 0.5-2 MB per run. Per-run evidence correctly reduces that handle's earned
// window from 5 blocks to 0 -- and the floor then hands it 2 blocks anyway, so the dispatch is
// unchanged and the arm does nothing. An external sweep measured exactly that: arm A identical
// to the default on all three walk shapes at ratio 2, which is a null with no information in it
// (the two arms are equal BY ARITHMETIC there). The floor, not the ratio, is what binds.
//
// So this arm converts the residual below the floor from blocks into BYTES: a handle that has
// earned less than the floor gets a span of exactly what it earned. At ratio 2, a 1 MB run
// earns 2 MB against the floor's 16 MiB.
//
// The 2x2 the two arms span, on the measured GRIB handle (ratio 2, 8 MiB blocks, lifetime
// 11.9 MB, runs ~1 MB) -- note that only the last row moves, and that neither arm alone does:
//
//	A off, B off -> window floors at 2 blocks            -> 16 MiB
//	A on,  B off -> window floors at 2 blocks            -> 16 MiB   (the measured null)
//	A off, B on  -> basis 11.9 MB earns 23.8 MB >= floor -> 16 MiB
//	A on,  B on  -> basis 1 MB earns 2 MB < floor        ->  2 MiB
//
// That is #222's precedent repeating: there, too, each half alone made bytes WORSE and only
// both together gave 16x. It is why these ship as two flags rather than one default.
//
// ★ THE PRECONDITION IS NOT A THRESHOLD. It requires runsEnded > 0 -- the handle has actually
// finished a run. Without it this fires on every handle's opening reads, where consumption is
// small because the handle has barely started, and #233 establishes that a cold sequential read
// should fetch MORE rather than less (#231 refuted three ways of making it fetch sooner). A
// sequential copy ends no runs, so it can never reach this. Today windowCap's floor is what
// protects that reader; this arm removes the floor, so it has to carry the protection itself.
//
// Returns ok=true when the bound is IN FORCE, so the caller must not fall through to a
// whole-block dispatch. The span's start offset is deliberately not expressed here: the
// detector reports how MUCH, and the FUSE layer -- which owns the read's byte offsets -- places
// it immediately after the read that produced it. Same split as Dispatch.ReadLen.
func (p *Prefetcher) subBlockSpan(blockIdx, readEnd int64) ([]Dispatch, bool) {
	if !p.subBlock || p.evidenceRatio <= 0 || p.evidenceBlock <= 0 {
		return nil, false
	}
	// EVIDENCE THAT THIS HANDLE IS NOT A STRAIGHT STREAM. Either reading is enough:
	//
	//   runsEnded > 0   -- it has finished an established run
	//   resetRandom > 0 -- it has collapsed to Random at least once
	//
	// The second was added because the first is almost never true of the real tools. An
	// external 2x2 found this arm inert on every measured shape, and the per-handle trace said
	// why: a GRIB handle establishes ONCE, in its last 12 of 90 reads, and finishes no runs at
	// all; 67 of its 90 reads are random->random. So "has finished a run" described a fixture,
	// and "has been Random" describes the tools.
	//
	// A plain sequential copy satisfies NEITHER. Verified in the detector: a copy reaches only
	// Cold then Sequential, with resetRandom 0 -- the #221 coverage gate holds its first reads
	// provisional as Cold, never Random. That is what keeps #233's cold streaming reader out.
	if p.runsEnded == 0 && p.resetRandom == 0 {
		return nil, false
	}
	// ★ THE BASIS HERE IS ALWAYS THE CURRENT RUN, not evidenceBasis(), and that is deliberate.
	//
	// This arm fires where a handle has demonstrated scattered reading, and at such a handle's
	// establishment its LIFETIME consumption is evidence about scattering rather than about
	// contiguous appetite -- GRIB's 11.8 MB lifetime earns 23.6 MB, above the floor, so a
	// lifetime basis declines on exactly the shape this exists for. The claim being bounded is
	// "how much contiguous reading has this handle shown in THIS stretch", so the basis has to
	// be the stretch.
	basis := p.runConsumed
	if basis <= 0 {
		return nil, false
	}
	earned := int64(float64(basis) * p.evidenceRatio)
	if earned >= initialWindow*p.evidenceBlock {
		// The floor is not what binds for this handle, so there is no residual to convert and
		// the normal whole-block path is already no larger than the evidence allows.
		return nil, false
	}

	// ★★ THE LEAD IS MAINTAINED, NOT PLACED ONCE. This is the correction a shipped regression
	// bought: the first version dispatched one span at establishment and nothing until the
	// reader crossed a block boundary, so a handle that went on STREAMING faulted through the
	// remainder. Measured 20% slower on an index-then-stream reader, below both brackets in 8
	// of 8 bracketed reps, with bytes identical -- the whole cost was faults and wall.
	//
	// The rule is the evidence-gate semantic expressed in bytes rather than blocks: keep the
	// frontier `earned` bytes ahead of the reader. `earned` grows with the run, so a handle
	// that keeps reading contiguously earns a deepening lead and converges on what the
	// whole-block path would have given it, while a handle that stops has committed only what
	// it had read when it stopped.
	want := readEnd + earned
	if want <= p.spanTo {
		// Already led this far. Returning ok=true matters: the bound is IN FORCE, so the caller
		// must not fall through and dispatch whole blocks instead.
		return nil, true
	}
	from := p.spanTo
	if from < readEnd {
		from = readEnd
	}
	p.spanTo = want
	p.spansIssued++
	return []Dispatch{{Block: blockIdx, SpanOff: from, SpanLen: want - from}}, true
}

// recordRead pushes a read's byte range into the trailing ring and returns the
// coverage ratio (union of ranges / total span) over the window. Returns 1.0
// when the gate is disabled or fewer than 3 reads have been seen (too little to
// judge; the initial ramp is left to the normal state machine).
func (p *Prefetcher) recordRead(off, length int64) float64 {
	if p.covWindow <= 1 || length <= 0 {
		return 1.0
	}
	p.covSpans = append(p.covSpans, covSpan{off, off + length})
	if len(p.covSpans) > p.covWindow {
		p.covSpans = p.covSpans[len(p.covSpans)-p.covWindow:]
	}
	if len(p.covSpans) < 3 {
		return 1.0
	}
	lo, hi := p.covSpans[0].off, p.covSpans[0].end
	for _, s := range p.covSpans[1:] {
		if s.off < lo {
			lo = s.off
		}
		if s.end > hi {
			hi = s.end
		}
	}
	span := hi - lo
	if span <= 0 {
		return 1.0
	}
	// Union of the (unsorted, possibly overlapping) ranges.
	sp := append([]covSpan(nil), p.covSpans...)
	sort.Slice(sp, func(i, j int) bool { return sp[i].off < sp[j].off })
	var union int64
	cs, ce := sp[0].off, sp[0].end
	for _, s := range sp[1:] {
		if s.off > ce {
			// A HOLE. Count it as covered when the bytes in it have already been
			// DEMANDED through lith -- a sibling read them (#316).
			//
			// FOPEN_KEEP_CACHE shares the inode page cache, so a sibling's reads are
			// served by the kernel and never reach this handle's stream. Its coverage
			// is then ~1/N and the #221 gate holds it provisional from N=4, which
			// measured 243x on 16 readers of one object with byte amplification of
			// 1.001 -- the counters all look clean. Discounting such a hole makes the
			// handle's coverage reflect what the FILE is doing rather than what this
			// descriptor was left to see.
			//
			// The predicate is deliberately "already demanded", not "resident": a
			// random walk's holes are not resident at all (#232 -- no lever, and
			// prefetching for one adds bytes without moving the wall), and lith's own
			// unread over-fetch is resident but undemanded, which must not count or a
			// strided reader would discount the regions its own window swept and
			// establish on them (#222's shape).
			if p.holeDemanded == nil || !p.holeDemanded(ce, s.off) {
				union += ce - cs
				cs, ce = s.off, s.end
				continue
			}
			ce = s.end
			continue
		}
		if s.end > ce {
			ce = s.end
		}
	}
	union += ce - cs
	return float64(union) / float64(span)
}

// SetHoleDemanded installs the predicate recordRead uses to decide whether a hole between
// two reads holds bytes that were already demanded through lith (#316). A nil predicate
// disables the discount, which is the default and reproduces the pre-#316 coverage exactly.
func (p *Prefetcher) SetHoleDemanded(fn func(off, end int64) bool) { p.holeDemanded = fn }

// SetGapMax sets the largest byte gap between consecutive reads that still counts
// as sequential progress (#210/M16 1b). The FUSE layer sets it to the block size:
// a read landing more than one block past the previous is a seek, not a stream,
// however in-band it looks in block space. n<=0 disables the gate (MaxInt64).
func (p *Prefetcher) SetGapMax(n int64) {
	if n <= 0 {
		n = math.MaxInt64
	}
	p.seqGapMax = n
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// State returns the current detected pattern.
func (p *Prefetcher) State() State { return p.state }

// strideRun is how many times a non-unit block delta must repeat before the handle is
// declared Strided (#222). Two repeats -- three consecutive reads at the same delta.
const strideRun = 2

// covReads is the minimum reads before coverage can judge tiling (#229). Below
// it the trailing window is too short to separate a stream from a walk, so the
// handle is treated as not-yet-established and served precise.
const covReads = 3

// coverageOK reports whether the coverage signal confirms tiling: the gate is on,
// enough reads have accumulated to judge, and the trailing coverage clears the
// threshold. cov is the ratio just returned by recordRead.
func (p *Prefetcher) coverageOK(cov float64) bool {
	if p.covMin <= 0 {
		return true // gate disabled: behaviour-preserving
	}
	return len(p.covSpans) >= covReads && cov >= p.covMin
}

// Established reports whether the access pattern is known to tile — the single
// gate the FUSE layer's open-time parts-fetch (#69/#220) and initial ramp (#228)
// consult before committing any broad fetch (#229). False until the coverage
// signal confirms tiling; false again if it later collapses.
func (p *Prefetcher) Established() bool { return p.established }

// SetMax updates the readahead window cap (in blocks). The FUSE layer lowers it
// when many handles are open so their windows share the prefetch budget (#55).
// The current window is clamped down to the new cap; the floor is initialWindow.
func (p *Prefetcher) SetMax(n int64) {
	if n < initialWindow {
		n = initialWindow
	}
	p.maxReadahead = n
	if p.window > n {
		p.window = n
	}
}

// Open sets up the handle but dispatches NO prefetch (#229): the initial ramp is
// a broad fetch, and the policy commits none until the access pattern is known to
// tile. The ramp begins from the first read that establishes coverage (read ~2),
// via Observe. With the coverage gate disabled it still dispatches the initial
// window, preserving the pre-#229 open-time ramp.
func (p *Prefetcher) Open() []Dispatch {
	p.haveLast = true
	p.lastBlock = -1 // so the first read at block 0 registers as sequential (d=1)
	p.state = Cold
	p.established = false
	if p.covMin > 0 {
		p.window = 0
		p.cursor = -1
		p.frontier = 0
		return nil
	}
	p.window = initialWindow
	p.cursor = -1
	p.frontier = initialWindow
	return blockRange(0, initialWindow)
}

// Dispatch is one block the detector is asking to be prefetched, plus what the detector knows
// about how much of it the reader is expected to want (#222).
//
// WHY A BLOCK INDEX IS NOT ENOUGH. The strided branch predicts blk+d, and that prediction is
// CORRECT about where a slice reader is going -- at stride d it is exactly the next read's
// block, every time. What it cannot express is that the reader wants 4 KiB of that block and
// not all 1 MiB of it, so the block was fetched whole and a FITS 2-D cutout paid 256x its read.
//
// ReadLen carries a FACT, not a policy: the length of the read that produced this prediction.
// The decision about what to do with it belongs to the FUSE layer, which already owns the
// identical judgement for demand reads (byteExactThreshold) and can therefore apply one rule
// in one place instead of two rules that drift.
type Dispatch struct {
	// Block is the block index to prefetch.
	Block int64
	// ReadLen is the length of the read that produced this prediction, when the prediction is
	// for a reader expected to want only part of the block. ZERO MEANS THE WHOLE BLOCK, which
	// is every sequential-ramp and seek-re-anchor dispatch: there, the reader has demonstrated
	// contiguous progress and the whole block is what it is going to consume.
	ReadLen int64
	// SpanOff and SpanLen, when SpanLen > 0, are an ABSOLUTE byte range to prefetch in place of
	// any whole block (#407 arm B). The length is an amount the handle's evidence has already
	// earned, NOT a read length to be judged against a threshold -- which is the difference from
	// ReadLen, and the reason these are separate fields. Applying ReadLen's small-read threshold
	// here would drop every span above that threshold back to a whole-block fetch, i.e. exactly
	// the shapes this arm is for.
	//
	// AN ABSOLUTE RANGE, NOT AN IMPLICIT "AFTER THIS READ". The first version carried only a
	// length and let the FUSE layer place it at the read's end, on the principle that the
	// detector reports how much and the layer owns offsets. That is elegant until a span has to
	// be EXTENDED as the reader consumes it -- the extension starts at the end of the previous
	// span, which only the detector knows. Keeping the lead required the detector to name the
	// range, and a shipped regression is what established that it has to keep the lead:
	// one span placed at establishment and never extended left a streaming reader faulting
	// until the next block crossing, measured 20% slower.
	//
	// Block is still set, for tracing and so a dispatch always says where it is.
	SpanOff, SpanLen int64
}

// Observe records a demand read at blockIdx, byteGap bytes from the end of the
// previous read on this handle, and returns the blocks to dispatch now
// (the frontier advance), empty when the frontier already leads far enough.
//
// byteGap distinguishes a stream from a scattered walk that the block-space
// in-band test alone cannot (#210/M16 1b): a metadata walk reads kilobytes at
// offsets megabytes apart, so its reads land in adjacent 8 MiB blocks (d==1) or
// inside the grown reorder band and were misread as sequential. A read whose
// |gap| exceeds seqGapMax is not sequential progress however in-band it looks —
// it falls through to the seek path. Contiguous streams (gap≈0) and kernel
// reorder (gap ≪ block) are unaffected; seqGapMax defaults to "off" (MaxInt64)
// so a caller that does not set it keeps the pre-1b behavior.
func (p *Prefetcher) Observe(blockIdx, off, length, byteGap int64) []Dispatch {
	// Demonstrated demand, accumulated before any gate so it is unaffected by
	// whether the coverage gate is enabled (#256).
	if length > 0 {
		p.consumed += length
		p.runConsumed += length
	}
	cov := p.recordRead(off, length)
	if !p.haveLast {
		p.haveLast = true
		p.lastBlock = blockIdx
		p.cursor = blockIdx
		return nil
	}
	d := blockIdx - p.lastBlock
	p.lastBlock = blockIdx

	// Re-read of the same block: no state change, and no WHOLE-BLOCK dispatch -- the frontier is
	// denominated in blocks and cannot move without a block advance.
	//
	// ★ BUT THE SPAN PATH IS DENOMINATED IN BYTES AND MUST STILL LEAD HERE. At the default
	// 8 MiB block a 128 KiB reader takes 64 reads to cross one, so returning early for all of
	// them is precisely what left a streaming handle faulting after its one span: the next
	// dispatch could not come until the crossing. Extending the lead needs forward progress,
	// which byteGap >= 0 is: a re-read of bytes already behind the reader gives a negative gap
	// and must not earn anything.
	if d == 0 {
		if byteGap >= 0 && p.established {
			if ds, ok := p.subBlockSpan(blockIdx, off+length); ok {
				return ds
			}
		}
		return nil
	}

	// Strided detection (before the seek rule): a constant non-unit delta repeated
	// strideRun times. Not evaluated once a sequential stream is established, so an in-band
	// reorder whose deltas happen to repeat is not misread as a stride.
	//
	// WHY A RUN AND NOT ONE REPEAT (#222). This branch has no byte-gap bound -- unlike the
	// sequential branch below, which requires absInt64(byteGap) <= seqGapMax -- and no
	// coverage check, and it runs before the seek rule on purpose, because a large gap is
	// what a stride IS. So its only evidence is the delta repeating, and one repeat is weak:
	// over a walk spanning B blocks, consecutive deltas collide by chance with probability
	// ~1/B, so N reads yield ~N/B false strides.
	//
	// That is structural rather than theoretical. A GENUINELY RANDOM mmap walk was measured
	// flipping Random -> Strided 7 times in 2471 reads, on coincidental gaps of +55 MB,
	// +166 MB and +131 MB, dispatching prefetch for +27 MB over distinct (#232's 5f-P11; the
	// prediction for 109 blocks and 2471 reads was ~23, so 7 is the same order). This
	// package's own random-walk harness had been reporting strided=1 and strided=3 over 281
	// and 1126 reads before anyone went looking.
	//
	// Requiring the delta to repeat twice takes the false-positive rate from ~1/B to ~1/B^2,
	// which turns that predicted ~23 into ~0.2. A real strided reader -- a FITS cutout
	// walking row segments, a hyperslab -- meets it trivially and establishes exactly one
	// read later.
	if d != 1 && d == p.lastDelta && p.state != Sequential {
		// The run gates ENTRY only. An already-strided handle must keep predicting on every
		// read: re-counting here made it dispatch on alternate reads, halving the branch's
		// usefulness for its legitimate users (11 predictions over 24 strided reads instead
		// of 21). Caught by TestStrideStillEstablishesForARealStride, which is the reason
		// that test asserts dispatch volume and not just the final state.
		if p.state != Strided {
			p.deltaRun++
			if p.deltaRun < strideRun {
				// Not yet enough evidence. Hold the delta so a third read can confirm it,
				// and dispatch nothing.
				p.lastDelta = d
				return nil
			}
		}
		p.state = Strided
		p.pending = false
		p.lastDelta = d
		p.deltaRun = 0
		p.cursor = blockIdx
		pred := blockIdx + d
		if pred >= p.frontier {
			p.frontier = pred + 1
			// ReadLen is this read's length (#222). A strided reader's next read is
			// overwhelmingly the same size as this one -- that is what makes it a stride --
			// so the length that produced the prediction is the best available statement of
			// how much of the predicted block will be wanted. A bulk strided reader taking
			// whole-chunk reads reports a whole-chunk length and is therefore unaffected,
			// which is the constraint TestStridedBulkReaderKeepsWholeChunks pins.
			return []Dispatch{{Block: pred, ReadLen: length}}
		}
		return nil
	}

	// Sequential progress: a unit step, or a read within the reorder band — but
	// only if the read is byte-contiguous. A large byte gap is a seek even when it
	// lands in an adjacent block or the grown band (#210/M16 1b: the scattered
	// metadata walk).
	if (d == 1 || p.inBand(blockIdx)) && absInt64(byteGap) <= p.seqGapMax {
		p.pending = false
		p.lastDelta = 1
		p.deltaRun = 0 // sequential progress breaks any stride run (#222)
		if blockIdx > p.cursor {
			p.cursor = blockIdx
		}
		// #229: recognize contiguous progress always, but commit a broad fetch
		// (the readahead ramp) only once coverage confirms the access tiles. Until
		// then the handle is provisional — the read is served precise and nothing
		// is dispatched — and a contiguous read whose window is still full of an
		// earlier scattered walk de-establishes rather than ramping.
		if !p.coverageOK(cov) {
			// Contiguous progress, refused a window. See the coverageHeld field comment:
			// this is the only place the #316 shape is observable, and it used to be silent.
			p.coverageHeld++
			p.deEstablish()
			if p.state == Sequential {
				p.state = Cold
			}
			p.window = 0
			p.frontier = p.cursor + 1
			return nil
		}
		justEstablished := p.state != Sequential
		if p.state == Sequential {
			p.window = min(p.window*2, p.windowCap())
		} else {
			// #229: on establishment jump straight to the full window rather than
			// re-ramping from 2. A genuine stream/copy pays only the first ~2 precise
			// reads; without this it also pays a geometric re-ramp (many small GETs,
			// an underfed NIC on the cold read — the #56 concern), which regressed
			// sequential copies. A handle that establishes then immediately seeks (a
			// per-field walk) reverts on the next landing, so the full window is
			// dispatched at most once per established run.
			p.state = Sequential
			if p.covMin > 0 {
				// #256: bounded by demonstrated consumption. With the evidence gate
				// off this is maxReadahead, i.e. the #229 jump, unchanged.
				p.window = p.windowCap()
			} else {
				p.window = initialWindow // gate off: pre-#229 geometric ramp
			}
		}
		p.established = true
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		// THE FIRST BLOCK PREFETCHED USED TO BE cursor+1, SKIPPING THE ONE THE READER IS IN.
		//
		// Establishment fires on the first BLOCK ADVANCE, so at that moment the reader has
		// just crossed into the current block and has most of it still ahead -- 63 more
		// 128 KiB reads of an 8 MiB block. Clamping the frontier to cursor+1 meant those were
		// all served as separate demand chunk GETs, so block 1 repeated block 0's cost
		// exactly (#233).
		//
		// Measured on a real mount with --timeline-csv: lag_ms is -1 for chunks 0-15 in every
		// one of 12 opens -- none of the first sixteen chunks was ever prefetched -- and the
		// reader is stalled 206-431 ms in block 0 and a further 245-389 ms in block 1, out of
		// a 569-957 ms read. The first prefetched block was block 2. Those two blocks are the
		// whole of the ~0.47 s per-open intercept that #284 could not account for.
		//
		// Block 0 is irreducible: establishment cannot fire before a block advance, and #231
		// refuted three ways to establish sooner. Block 1 is not -- the reader is one chunk
		// into it and the other seven are ahead. Dispatching from the cursor rather than past
		// it costs nothing when it is already covered, because the chunk singleflight joins a
		// demand fill already in flight instead of duplicating it.
		//
		// Only at establishment. A handle mid-stream has its frontier well ahead, so the
		// clamp never fires for it, and re-dispatching a block the reader has nearly finished
		// would be waste rather than lead.
		first := p.cursor + 1
		if justEstablished {
			first = p.cursor
		}
		if p.frontier < first {
			p.frontier = first
		}
		// #407 arm B: a handle that has proved it takes short runs gets a span of what it
		// earned rather than the floor's two whole blocks. Inert unless the flag is on AND the
		// handle has ended a run AND the floor is what binds -- see subBlockSpan.
		if ds, ok := p.subBlockSpan(p.cursor, off+length); ok {
			return ds
		}
		return p.advance(p.cursor + 1 + p.window)
	}

	// Out of band: a seek. The run resets because this delta is, by construction, not the one
	// being counted -- the strided branch above already consumed the matching case. Without
	// this, a match at delta X followed by a non-match then two matches at delta Y would
	// reach strideRun on three reads that were never consecutive (#222).
	p.lastDelta = d
	p.deltaRun = 0
	// Coverage gate (#221): a seek landing whose trailing coverage is low is a
	// scattered walk (metadata traversal, GRIB .idx field sweep, COG overview),
	// not a stream taking one jump. Do NOT optimistically re-anchor Sequential and
	// prefetch two blocks at the landing — that re-anchor, fired at every landing,
	// refilled whole sub-64-MiB objects (#220). Go Random and dispatch nothing; a
	// walk that later tiles re-establishes Sequential via the contiguous branch
	// above once coverage recovers. Streams never trip this (coverage ~1, and they
	// rarely reach the seek path at all). Disabled by default (covMin<=0).
	if p.covMin > 0 && cov < p.covMin {
		if p.state != Random {
			p.resetRandom++
		}
		p.lowCoverage++
		p.state = Random
		p.deEstablish() // #229: a scattered landing de-establishes the handle
		p.pending = false
		p.window = 0
		p.cursor = blockIdx
		p.frontier = blockIdx
		return nil
	}
	if p.pending {
		// A second unexplained jump with no sequential progress between: stop
		// prefetching until a sequential run resumes.
		p.state = Random
		p.deEstablish()
		p.pending = false
		p.window = 0
		p.cursor = blockIdx
		p.frontier = blockIdx
		p.resetRandom++
		return nil
	}
	if p.state == Sequential || p.state == Strided {
		// One jump out of an established pattern: halve the window (floor 2) —
		// not a full reset to zero, which is the #49 lone-survivor re-ramp — and
		// re-anchor at b with the initial 2-block dispatch.
		p.window = max(initialWindow, p.window/2)
		p.halvings++
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		p.state = Sequential
		p.pending = true
		p.cursor = blockIdx
		p.frontier = blockIdx
		// The run ended here even though the handle stays Sequential (#407). This is the branch
		// a field-at-a-time walk takes between fields, and leaving runConsumed alone would make
		// per-run evidence identical to cumulative evidence for the exact shape it exists for.
		//
		// ARM B IS EVALUATED BEFORE endRun, DELIBERATELY. endRun zeroes runConsumed, which is
		// arm A's basis, so computing the span afterwards would read a basis of 0 and fall
		// through to two whole blocks on exactly the landing this arm exists to bound -- the
		// two arms would then compose to nothing on the branch where they matter most. The
		// evidence for this landing is the run that just finished, which is what runConsumed
		// still holds here.
		ds, bounded := p.subBlockSpan(blockIdx, off+length)
		p.endRun()
		if bounded {
			return ds
		}
		return p.advance(blockIdx + initialWindow)
	}
	// From Cold/Random with no pending jump: tentative — wait to see if this is
	// a stride or a seek before committing.
	p.pending = true
	return nil
}

// inBand reports whether b is within the reorder band around the current
// cursor/frontier: [cursor-window, frontier+window], with at least the initial
// window of slack so re-entry from a zero window still recognises progress.
func (p *Prefetcher) inBand(b int64) bool {
	w := p.window
	if w < initialWindow {
		w = initialWindow
	}
	return b >= p.cursor-w && b <= p.frontier+w
}

// advance dispatches [frontier, target) and moves the frontier to target.
func (p *Prefetcher) advance(target int64) []Dispatch {
	if target <= p.frontier {
		return nil
	}
	lo := p.frontier
	p.frontier = target
	return blockRange(lo, target-lo)
}

// blockRange returns [start, start+count).
// blockRange is a whole-block dispatch run: every entry has ReadLen 0, because a frontier
// advance is only ever issued to a handle that has demonstrated contiguous progress.
func blockRange(start, count int64) []Dispatch {
	if count <= 0 {
		return nil
	}
	out := make([]Dispatch, count)
	for i := int64(0); i < count; i++ {
		out[i] = Dispatch{Block: start + i}
	}
	return out
}
