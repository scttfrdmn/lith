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
