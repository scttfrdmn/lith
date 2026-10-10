// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// fillSecRec is a Recorder that also implements fillLatencyRecorder, keeping one sample per
// kind (#362).
type fillSecRec struct {
	mu     sync.Mutex
	byKind map[string]int
	gets   int64
}

func (r *fillSecRec) MemHit()                   {}
func (r *fillSecRec) DiskHit()                  {}
func (r *fillSecRec) Miss()                     {}
func (r *fillSecRec) StartInflight()            {}
func (r *fillSecRec) EndInflight()              {}
func (r *fillSecRec) StaleKey(string)           {}
func (r *fillSecRec) PrefetchIssued()           {}
func (r *fillSecRec) PrefetchHit()              {}
func (r *fillSecRec) UncoveredMiss()            {}
func (r *fillSecRec) S3Get(n int64, isErr bool) { r.mu.Lock(); r.gets++; r.mu.Unlock() }

func (r *fillSecRec) FillSeconds(kind string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKind == nil {
		r.byKind = map[string]int{}
	}
	r.byKind[kind]++
}

func (r *fillSecRec) got() (map[string]int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for k, v := range r.byKind {
		out[k] = v
	}
	return out, r.gets
}

// #362: every fill's first-byte latency reaches the Recorder LABELLED BY KIND, so an operator
// can see what a fault costs without reading the source.
//
// The reporter lost most of a day to a 1.2 TB mount that was working correctly in #232's
// regime, because "the per-fault latency, the fill kind, and the random-vs-sequential
// classification are all things lith knows and I couldn't see."
//
// ASSERTS ONE SAMPLE PER FILL, not "> 0". The ttfbRecorder hook beside this one shipped DEAD —
// declared, never called, through a clean build, a clean lint and a green test run — and a
// `> 0` assertion would pass on a single sample. A hook firing once per handle rather than
// once per fill is the #319 defect's shape.
func TestEveryFillsLatencyReachesTheRecorderLabelled(t *testing.T) {
	srv := fake.New()
	const objSize = 24 << 20
	body := make([]byte, objSize)
	for i := range body {
		body[i] = byte(i * 31 % 251)
	}
	srv.Put("obj", body, time.Unix(1_700_000_000, 0))

	rec := &fillSecRec{}
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	k := keyFor(t, srv, "obj")

	if byKind, _ := rec.got(); len(byKind) != 0 {
		t.Fatalf("samples before any fill: %v", byKind)
	}

	const readLen = int64(64) << 10
	for off := int64(0); off < objSize; off += readLen {
		b, err := bs.GetRange(context.Background(), k, off, readLen, objSize)
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		// Verify the bytes, so a fixture that serves nothing cannot make the counts below
		// meaningless.
		if len(b) != int(readLen) || b[0] != byte(off*31%251) {
			t.Fatalf("read at %d returned %d bytes, first %d", off, len(b), b[0])
		}
	}

	byKind, gets := rec.got()
	total := 0
	for _, n := range byKind {
		total += n
	}
	if total == 0 {
		t.Fatal("no fill latency reached the Recorder: the fillLatencyRecorder hook is not " +
			"wired into recordTTFB, so lith_fill_seconds would be an always-empty histogram " +
			"and #362's diagnosis would still be unavailable")
	}
	// ONE SAMPLE PER FILL. Tier hits do not fill, so the count tracks GETs and not reads.
	if int64(total) != gets {
		t.Errorf("observed %d labelled latencies against %d GETs; the hook does not fire once "+
			"per fill (by kind: %v)", total, gets, byKind)
	}
	if gets < 2 {
		t.Fatalf("fixture produced %d GETs; a one-fill fixture cannot distinguish per-fill "+
			"from per-handle", gets)
	}
	// THE LABEL SAYS WHAT LITH DID, AND "demand" MEANS UNCOVERED -- not "scattered".
	// GetRange is the demand path and labels fillDemand whether or not the access is
	// sequential, because the fill happened with no prediction covering it. That is the right
	// quantity for #362: a mount in the fault-stream regime is ~100% demand, a healthy
	// streaming mount is mostly `whole` from prefetch with a few demand fills at its cold
	// start. These reads are driven directly with no prefetcher, so they are all demand.
	if byKind["demand"] != total {
		t.Errorf("reads driven straight through GetRange produced %v; all %d should be "+
			"kind=demand, since nothing prefetched for them", byKind, total)
	}
}

// The other half of the label's value: a PREFETCH fill must be labelled `whole`, or the
// metric cannot distinguish a mount that is streaming from one that is faulting -- which is
// the single distinction #362 asked for.
func TestPrefetchFillsAreLabelledWhole(t *testing.T) {
	srv := fake.New()
	const objSize = 16 << 20
	body := make([]byte, objSize)
	for i := range body {
		body[i] = byte(i * 17 % 253)
	}
	srv.Put("obj", body, time.Unix(1_700_000_000, 0))

	rec := &fillSecRec{}
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	k := keyFor(t, srv, "obj")

	for blk := int64(0); blk < 8; blk++ {
		bs.Prefetch(context.Background(), k, blk, objSize)
	}

	byKind, gets := rec.got()
	if byKind["whole"] == 0 {
		t.Fatalf("prefetch produced %v with no kind=whole: the metric cannot tell a "+
			"streaming mount from a faulting one, which is the whole point of #362", byKind)
	}
	if byKind["demand"] != 0 {
		t.Errorf("prefetch fills were labelled demand: %v — the label would then be useless "+
			"for the distinction it exists to make", byKind)
	}
	total := 0
	for _, n := range byKind {
		total += n
	}
	if int64(total) != gets {
		t.Errorf("observed %d labelled latencies against %d GETs (by kind: %v)",
			total, gets, byKind)
	}
	if gets < 2 {
		t.Fatalf("fixture produced %d GETs", gets)
	}
}

// #222's fix must be OBSERVABLE: an extent-granular strided prefetch is labelled
// `prefetch-range`, distinct from the `whole` that a block prefetch reports. The point of the
// separate label is that the bytes MOVE from one to the other when the fix engages, so a
// shared label would hide it -- and a label nothing emits is a label that lies by omission.
func TestPrefetchRangeIsLabelledDistinctly(t *testing.T) {
	srv := fake.New()
	const objSize = 16 << 20
	body := make([]byte, objSize)
	for i := range body {
		body[i] = byte(i * 17 % 253)
	}
	srv.Put("obj", body, time.Unix(1_700_000_000, 0))

	rec := &fillSecRec{}
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	k := keyFor(t, srv, "obj")

	// Eight extent-granular prefetches, one per distinct chunk — the shape the strided
	// branch now dispatches.
	for i := int64(0); i < 8; i++ {
		bs.PrefetchRange(context.Background(), k, i*(2<<20)+1234, 4<<10, objSize)
	}

	byKind, gets := rec.got()
	if byKind["prefetch-range"] == 0 {
		t.Fatalf("PrefetchRange produced %v with no kind=prefetch-range: #222's fix would be "+
			"invisible in lith_fill_bytes_total and lith_fill_seconds", byKind)
	}
	if byKind["whole"] != 0 {
		t.Errorf("an extent prefetch was labelled whole: %v — the two would be "+
			"indistinguishable and the byte movement #222 is about could not be seen", byKind)
	}
	total := 0
	for _, n := range byKind {
		total += n
	}
	if int64(total) != gets {
		t.Errorf("observed %d labelled latencies against %d GETs (by kind: %v)", total, gets, byKind)
	}

	// AND IT MUST ACTUALLY FETCH LESS THAN A BLOCK PREFETCH WOULD. The same eight chunks
	// through Prefetch, measured on the SAME server with before/after snapshots so the two
	// phases cannot contaminate each other -- one server shared by two stores would pool the
	// byte counts and this comparison would be meaningless.
	rangeBytes := srv.GetByteCount()

	rec2 := &fillSecRec{}
	bs2 := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec2})
	for i := int64(0); i < 8; i++ {
		bs2.Prefetch(context.Background(), k, i*2, objSize)
	}
	wholeBytes := srv.GetByteCount() - rangeBytes
	if wholeBytes == 0 {
		t.Fatal("the whole-block control fetched nothing")
	}
	t.Logf("eight chunks: PrefetchRange %d B, Prefetch %d B (%.1fx)", rangeBytes, wholeBytes,
		float64(wholeBytes)/float64(rangeBytes))
	if rangeBytes >= wholeBytes {
		t.Errorf("PrefetchRange fetched %d B against the whole-block path's %d B — it is not "+
			"bounding anything", rangeBytes, wholeBytes)
	}
}

// THE ACCOUNTING CLAIM, which is load-bearing for the pressure gate (#313) and was only a
// comment until now: PrefetchRange commits the WANTED EXTENT SPAN, not a whole chunk per
// chunk. Charging a full chunk for a 64 KiB extent would over-report committed by up to 16x,
// and committed is exactly what --prefetch-pressure-max admits against — so the error would
// make the gate refuse prefetch on a mount that had committed almost nothing.
func TestPrefetchRangeCommitsOnlyItsExtents(t *testing.T) {
	srv := fake.New()
	const objSize = 16 << 20
	srv.Put("obj", make([]byte, objSize), time.Unix(1_700_000_000, 0))
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MemCache: 256 << 20, MaxRange: 64 << 20})
	k := keyFor(t, srv, "obj")

	const probes = int64(8)
	for i := int64(0); i < probes; i++ {
		bs.PrefetchRange(context.Background(), k, i*(2<<20)+1234, 4<<10, objSize)
	}
	committed := bs.PrefetchCommittedBytes()

	// One extent per probe is the expected commitment; a whole chunk per probe is the bug.
	wantMax := probes * int64(ExtentSize)
	chunkCharge := probes * int64(ChunkSize)
	t.Logf("%d extent prefetches committed %d B (one extent each would be %d, one chunk each %d)",
		probes, committed, wantMax, chunkCharge)
	if committed > wantMax {
		t.Errorf("committed %d B for %d extent prefetches, above %d B (one extent each) — the "+
			"charge is chunk-granular, which over-reports the quantity the #313 pressure gate "+
			"admits against by up to %.0fx", committed, probes, wantMax,
			float64(chunkCharge)/float64(wantMax))
	}
	if committed <= 0 {
		t.Errorf("committed %d B — an extent prefetch must still be charged, or the pressure "+
			"gate cannot see it at all", committed)
	}
}

// THE PRESSURE GATE MUST COVER PrefetchRange TOO (#313 + #222), and nothing caught it when I
// first wrote the function without it — found by a revert sweep, not by reading.
//
// An extent prefetch is still speculation committed against the tier. A second prefetch entry
// point that skipped the gate would be an admission-control hole scaling with however many
// strided readers a mount has, and the gauge the gate reads would look fine throughout,
// because the bypassing path still charges `committed`.
//
// IT DRIVES `committed` DIRECTLY RATHER THAN RACING TO RAISE IT. The first version of this
// test dispatched 64 prefetches concurrently behind a GET delay and asserted some were held:
// it got 31 holds locally and ZERO in CI, because whether commitment accumulates depends on
// how many dispatches overlap before any lands, which is the scheduler's business. That is a
// tight assertion on a load-sensitive quantity — the same mistake as the #313 gate test that
// asserted the gate HALVES peak pressure and then flaked in the full-tree run. The question
// here is narrow and deserves a narrow test: does PrefetchRange CONSULT the gate?
func TestPrefetchRangeObeysThePressureGate(t *testing.T) {
	run := func(t *testing.T, pressureMax float64) (held int64, gets int) {
		t.Helper()
		srv := fake.New()
		const objSize = 16 << 20
		srv.Put("obj", make([]byte, objSize), time.Unix(1_700_000_000, 0))
		rec := &pressureRec{}
		bs := newStore(t, srv, Config{
			BlockSize: 1 << 20, MemCache: 8 << 20, MaxRange: 64 << 20,
			PrefetchPressureMax: pressureMax, Recorder: rec,
		})
		k := keyFor(t, srv, "obj")
		// Pressure 0.9, above the 0.85 threshold, set outright. Nothing has to race.
		if cap := bs.MemCap(); cap > 0 {
			bs.pfCommittedBytes.Add(int64(0.9 * float64(cap)))
		} else {
			t.Fatal("fixture has no memory tier, so the gate's denominator is zero")
		}
		start := srv.GetCallCount()
		bs.PrefetchRange(context.Background(), k, 1234, 4<<10, objSize)
		return rec.held.Load(), srv.GetCallCount() - start
	}

	held, gets := run(t, 0.85)
	t.Logf("gate at 0.85 with pressure pre-set to 0.90: held=%d, GETs=%d", held, gets)
	if held != 1 {
		t.Errorf("held %d dispatches, want 1 — PrefetchRange is bypassing #313's admission "+
			"control, which is a hole that scales with the number of strided readers on the "+
			"mount", held)
	}
	if gets != 0 {
		t.Errorf("the refused prefetch still issued %d GET(s); the gate must drop the "+
			"dispatch, not merely count it", gets)
	}

	// THE CONTROL, without which the assertion above could pass for an unrelated reason: with
	// the gate off, the identical fixture holds nothing and does fetch.
	offHeld, offGets := run(t, 0)
	t.Logf("gate off, same pressure: held=%d, GETs=%d", offHeld, offGets)
	if offHeld != 0 {
		t.Errorf("the gate held %d dispatches with PrefetchPressureMax=0", offHeld)
	}
	if offGets == 0 {
		t.Error("with the gate off the prefetch fetched nothing, so the comparison above " +
			"measures something other than the gate")
	}
}
