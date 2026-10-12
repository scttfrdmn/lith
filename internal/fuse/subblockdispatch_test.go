// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// kindRec counts fills by kind. It satisfies blockstore's unexported fillLatencyRecorder by
// method set, which is the only hook that reports WHICH PATH fetched a byte -- and that, not
// the byte total, is what distinguishes this mechanism from the several other things that also
// make a walk fetch less.
type kindRec struct {
	mu     sync.Mutex
	byKind map[string]int
	// uncovered counts demand reads whose chunk was neither cached nor in flight -- a read the
	// readahead frontier did not lead far enough to cover. It is the LEADING INDICATOR for
	// whether bounding a dispatch costs wall clock: bytes a reader never wanted are pure
	// waste, but bytes it did want and now has to fault for are a stall. The byte count alone
	// cannot tell those apart, which is why the wall-clock question is open at all.
	uncovered int
	pfHit     int
	pfIssued  int
}

func (r *kindRec) MemHit()           {}
func (r *kindRec) DiskHit()          {}
func (r *kindRec) Miss()             {}
func (r *kindRec) StartInflight()    {}
func (r *kindRec) EndInflight()      {}
func (r *kindRec) StaleKey(string)   {}
func (r *kindRec) PrefetchIssued()   { r.mu.Lock(); r.pfIssued++; r.mu.Unlock() }
func (r *kindRec) PrefetchHit()      { r.mu.Lock(); r.pfHit++; r.mu.Unlock() }
func (r *kindRec) UncoveredMiss()    { r.mu.Lock(); r.uncovered++; r.mu.Unlock() }
func (r *kindRec) S3Get(int64, bool) {}

func (r *kindRec) FillSeconds(kind string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKind == nil {
		r.byKind = map[string]int{}
	}
	r.byKind[kind]++
}

func (r *kindRec) coverage() (uncovered, hit, issued int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uncovered, r.pfHit, r.pfIssued
}

func (r *kindRec) counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for k, v := range r.byKind {
		out[k] = v
	}
	return out
}

// newWalkFS is newByteExactFS with the #407 knobs settable and a kind-counting Recorder, so
// the arms can be measured end to end on real bytes AND on which path fetched them.
func newWalkFS(t *testing.T, objBytes, blockSz int64, ratio float64, perRun, subBlock bool) (*rawFS, *fake.Server, *kindRec) {
	t.Helper()
	srv := fake.New()
	srv.Put("big.bin", bytes.Repeat([]byte("A"), int(objBytes)), time.Unix(1_700_000_000, 0))
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	rec := &kindRec{}
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: blockSz, MemCache: 512 << 20, MaxRange: 64 << 20, Recorder: rec,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(256<<20, prefetch.DeviceLimits{
		NICBytesPerSec: 100 << 20, TTFB: 10 * time.Millisecond,
	}, nil)
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000, Limits: lim,
		ReadaheadEvidenceRatio:  ratio,
		ReadaheadEvidencePerRun: perRun,
		ReadaheadSubBlock:       subBlock,
	}).(*rawFS)
	return raw, srv, rec
}

// initialWindowBytes is the smallest whole-block dispatch the evidence gate can produce -- the
// 2-block floor, in bytes. Spelled out because internal/prefetch does not export it and the
// fixture below has to assert it is clearing it.
func initialWindowBytes(blockSz int64) int64 { return 2 * blockSz }

// ★ #407 arm B, END TO END: a sub-block span reaches S3 as a RANGE, and the pair moves bytes.
//
// THE PATH THIS EXERCISES IS RE-ESTABLISHMENT, not the seek re-anchor, and that is forced
// rather than chosen. `coverageMin()` cannot return 0 -- a Config value of 0 or below falls
// back to the shipped 0.5 -- so a mount always has the #221 coverage gate on, and with it on
// the re-anchor branch is unreachable for any shape this arm can act on:
//
//	cov >= covMin at the landing  needs  field >= gap  (union/span over the trailing window)
//	gap > seqGapMax               needs  gap   >  blockSize
//	=> field > blockSize          =>     earned = field*ratio > the 2-block floor => inert
//
// What IS reachable is what an external trace of the real GRIB shape shows -- 12 sequential
// transitions against 67 random ones: a scattered landing goes Random and de-establishes, the
// field's own contiguous reads then re-establish the handle, and that establishment dispatches
// a full window. This arm bounds that dispatch.
func TestSubBlockDispatchFetchesARangeNotBlocks(t *testing.T) {
	const objBytes = int64(48) << 20
	const blockSz = int64(4) << 20 // the fixture's --block-size; see the chunk-reach note below
	const ratio = 2.0              // the in-region default
	const spacing = int64(8) << 20 // gap > seqGapMax = blockSize, so a jump is a seek
	const fields = 5

	// ★ THE READ SIZE AND THE CROSSING POINT ARE DERIVED, and the window is narrow enough that
	// picking them by eye produces a fixture that measures the wrong thing. Three constraints:
	//
	//  1. preCross >= coverageWindow*read -- the re-establishing read is the one that crosses a
	//     block boundary, and it re-establishes only if the 16-read coverage window has already
	//     refilled with THIS field's reads. Crossing sooner leaves the window spanning the
	//     previous field across a 4 MiB gap, coverage ~0.1, and the handle held provisional. A
	//     first version crossed at read 10 and reported zero spans in every arm.
	//  2. preCross*ratio < blockSize -- the span must be SHORTER THAN ONE BLOCK or this is not
	//     testing sub-block dispatch. A second version had a 1.5 MiB span against a 1 MiB
	//     block: the bytes still fell, but only because one dispatch replaced three, and
	//     unwiring the entire FUSE half left the test passing.
	//  3. Together they need read < blockSize/(ratio*coverageWindow) = 32 KiB here. At the
	//     shipped defaults -- 8 MiB blocks, 128 KiB reads -- that bound is 256 KiB, so a real
	//     mount sits well inside it; this fixture's small block is what makes it tight.
	//  4. AND THE BLOCK MUST BE SEVERAL CHUNKS. A span starts just past the read that earned
	//     it, so its first bytes fall in the chunk that read is already being served from --
	//     PrefetchRange dedups those, correctly. For the span to reach anything NEW the block
	//     has to span more chunks than the span's own start consumes, which needs
	//     blockSize >= 4*ChunkSize: at 1 MiB and 2 MiB blocks a span reaches 0 new chunks and
	//     the arm is structurally incapable of fetching anything. A third version of this
	//     fixture used a 1 MiB block, issued 6 spans, dropped bytes 2.00x -- and produced ZERO
	//     prefetch-range fills, because the "saving" was prefetch doing nothing at all. At the
	//     shipped 8 MiB default a span reaches 3 new chunks.
	const read = int64(32) << 10
	const preCross = int64(1) << 20
	const field = preCross + 256<<10

	if preCross < coverageWindow*read {
		t.Fatalf("fields cross a block boundary %d KiB in, before the %d-read coverage window "+
			"(%d KiB) refills from this field alone — the crossing read is held provisional and "+
			"the handle never re-establishes", preCross>>10, coverageWindow,
			(coverageWindow*read)>>10)
	}
	span := int64(float64(preCross) * ratio)
	if span >= blockSz {
		t.Fatalf("a %d KiB run earns a %d KiB span at ratio %g, not under the %d KiB block — "+
			"this fixture would measure fewer dispatches rather than sub-block ones",
			preCross>>10, span>>10, ratio, blockSz>>10)
	}
	if span >= initialWindowBytes(blockSz) {
		t.Fatalf("a %d KiB span is not under the %d KiB two-block floor, so there is no "+
			"residual to convert", span>>10, initialWindowBytes(blockSz)>>10)
	}
	if reach := blockSz/int64(blockstore.ChunkSize) - 1; reach < 1 {
		t.Fatalf("a %d MiB block is %d chunks, so a span starting just past its read reaches "+
			"%d new chunks — PrefetchRange would dedup the whole span and the arm could not "+
			"fetch anything", blockSz>>20, blockSz/int64(blockstore.ChunkSize), reach)
	}

	walk := func(perRun, subBlock bool) (fetched, spans int64, kinds map[string]int, st prefetch.State) {
		raw, srv, rec := newWalkFS(t, objBytes, blockSz, ratio, perRun, subBlock)
		node, fh := openBig(t, raw)
		h := raw.handleOf(fh)
		before := quiesce(srv)
		for f := int64(0); f < fields; f++ {
			// Straddle a block boundary so the field produces a block advance; a field wholly
			// inside one block yields only d==0 reads, which return before any state change.
			base := f*spacing + blockSz - preCross
			for off := base; off < base+field; off += read {
				readAt(t, raw, node, fh, off, read)
			}
		}
		// THE LEADING INDICATOR FOR THE WALL-CLOCK QUESTION, logged and deliberately NOT
		// asserted. Bytes a reader never wanted are pure waste; bytes it did want and now has
		// to fault for are a stall, and the byte count cannot tell those apart. On this
		// fixture the arm halves bytes and TRIPLES uncovered misses (4 -> 12) while prefetch
		// hits fall 6 -> 1, so it is a bytes-for-faults trade whose sign depends on bandwidth
		// and first-byte latency -- break-even is around 100 MB/s at a 30 ms first byte if the
		// faults serialise. An external run measured the GET count FLAT (72 -> 71) on the real
		// shape, which argues the fault increase does not transfer; the two disagree and only
		// a wall-clock cell on the real workload settles it.
		//
		// No assertion, because any bound here would be a tuned constant on a fixture-specific
		// ratio -- the mistake #340 and #349 both shipped. The figures are logged so a future
		// change to this arm shows its effect on faults, not only on bytes.
		unc, hit, iss := rec.coverage()
		t.Logf("    subBlock=%-5v uncovered=%d prefetchHit=%d prefetchIssued=%d", subBlock, unc, hit, iss)
		return quiesce(srv) - before, h.pf.pf.SpansIssued(), rec.counts(), h.pf.state()
	}

	baseBytes, baseSpans, baseKinds, baseState := walk(false, false)
	aBytes, aSpans, _, _ := walk(true, false)
	abBytes, abSpans, abKinds, abState := walk(true, true)

	wanted := int64(fields) * field
	t.Logf("%d fields of %d KiB in %d KiB reads over %d KiB blocks; %d KiB wanted:",
		fields, field>>10, read>>10, blockSz>>10, wanted>>10)
	t.Logf("  A off B off  %8d B (%5.2fx wanted)  spans=%d  state=%v  fills=%v",
		baseBytes, float64(baseBytes)/float64(wanted), baseSpans, baseState, baseKinds)
	t.Logf("  A on  B off  %8d B (%5.2fx wanted)  spans=%d",
		aBytes, float64(aBytes)/float64(wanted), aSpans)
	t.Logf("  A on  B on   %8d B (%5.2fx wanted)  spans=%d  state=%v  fills=%v",
		abBytes, float64(abBytes)/float64(wanted), abSpans, abState, abKinds)

	// FIXTURE PRECONDITIONS: it must over-fetch, or there is nothing to bound.
	if baseBytes <= wanted {
		t.Fatalf("the baseline fetched %d B for %d B wanted — it is not over-fetching, so "+
			"nothing below can show an improvement", baseBytes, wanted)
	}
	if abSpans == 0 {
		t.Fatalf("both arms issued no spans; the walk is not reaching the mechanism (most " +
			"likely it never re-establishes and sits in Random dispatching nothing)")
	}
	if baseSpans != 0 || aSpans != 0 {
		t.Errorf("spans issued without arm B: %d baseline, %d arm A alone", baseSpans, aSpans)
	}

	// ★★ THE ASSERTION THAT PINS THE MECHANISM: the span is fetched as a RANGE.
	//
	// A byte total cannot do this job, and a revert sweep proved it. Unwiring the FUSE half
	// leaves the detector returning one span dispatch where it returned three blocks; that
	// falls through to a whole-block Prefetch of ONE block, so the bytes still drop and a
	// "fewer bytes" assertion passes with half the fix removed. Judging the span against the
	// small-read threshold instead DROPS the prefetch entirely, which also reduces bytes.
	// Both reverts are caught here and by nothing else.
	const rangeKind = "prefetch-range"
	if abKinds[rangeKind] == 0 {
		t.Errorf("both arms on produced no kind=%q fills (%v). The span is being fetched as a "+
			"whole block, or not at all — the detector's half is wired and the fetch path's "+
			"half is not", rangeKind, abKinds)
	}
	if baseKinds[rangeKind] != 0 {
		t.Errorf("the baseline produced %d kind=%q fills; this fixture has no strided reader, "+
			"so that label can only come from arm B and the baseline must have none",
			baseKinds[rangeKind], rangeKind)
	}

	// AND THE BYTES, which is what an operator pays.
	if abBytes >= baseBytes {
		t.Errorf("both arms fetched %d B against the baseline's %d B", abBytes, baseBytes)
	} else {
		t.Logf("  => %.2fx fewer bytes off S3", float64(baseBytes)/float64(abBytes))
	}
	// A floor, so a fixture serving from the tier cannot pass quietly.
	if abBytes < wanted {
		t.Errorf("both arms fetched %d B for %d B of reads — below what the reader demanded, "+
			"so this measures cache hits rather than fetches", abBytes, wanted)
	}
}

// Arm B must not change a mount that does not enable it. Asserted on BYTES through the whole
// stack rather than on the detector's flag, because Config-to-detector plumbing is exactly
// what has shipped dead in this repo before.
func TestSubBlockDispatchIsOffByDefaultEndToEnd(t *testing.T) {
	const objBytes = int64(16) << 20
	const read = int64(128) << 10

	run := func(cfgOn bool) (int64, int64, map[string]int) {
		raw, srv, rec := newWalkFS(t, objBytes, 1<<20, 2.0, false, cfgOn)
		node, fh := openBig(t, raw)
		h := raw.handleOf(fh)
		before := quiesce(srv)
		for off := int64(0); off+read < objBytes; off += read {
			readAt(t, raw, node, fh, off, read)
		}
		return quiesce(srv) - before, h.pf.pf.SpansIssued(), rec.counts()
	}

	off, offSpans, offKinds := run(false)
	on, onSpans, onKinds := run(true)
	t.Logf("whole-object sequential read: flag off %d B / %d spans / %v", off, offSpans, offKinds)
	t.Logf("                              flag on  %d B / %d spans / %v", on, onSpans, onKinds)
	if off != on {
		t.Errorf("a plain sequential reader fetched %d B with the flag off and %d B with it on; "+
			"a streaming reader must be untouched (#231/#233)", off, on)
	}
	if offSpans != 0 || onSpans != 0 {
		t.Errorf("a sequential reader was given spans: %d off, %d on", offSpans, onSpans)
	}
	if onKinds["prefetch-range"] != 0 {
		t.Errorf("a sequential reader produced %d prefetch-range fills with the flag on",
			onKinds["prefetch-range"])
	}
}
