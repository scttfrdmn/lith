// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// ttfbRec is a Recorder that also implements ttfbRecorder, keeping every sample it is
// handed (#341).
type ttfbRec struct {
	mu      sync.Mutex
	samples []time.Duration
	gets    int64
}

func (r *ttfbRec) MemHit()                   {}
func (r *ttfbRec) DiskHit()                  {}
func (r *ttfbRec) Miss()                     {}
func (r *ttfbRec) StartInflight()            {}
func (r *ttfbRec) EndInflight()              {}
func (r *ttfbRec) StaleKey(string)           {}
func (r *ttfbRec) PrefetchIssued()           {}
func (r *ttfbRec) PrefetchHit()              {}
func (r *ttfbRec) UncoveredMiss()            {}
func (r *ttfbRec) S3Get(n int64, isErr bool) { r.mu.Lock(); r.gets++; r.mu.Unlock() }

func (r *ttfbRec) S3TTFB(d time.Duration) {
	r.mu.Lock()
	r.samples = append(r.samples, d)
	r.mu.Unlock()
}

func (r *ttfbRec) got() ([]time.Duration, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.samples...), r.gets
}

// #341: every fill's first-byte latency reaches the Recorder, so the DISTRIBUTION is
// exportable and not only the rolling median the evidence policy reads.
//
// This test exists because the hook shipped DEAD once already, in this same change: the
// interface was declared next to prefetchWaitRecorder but the call was never added to
// recordTTFB, which lives in another file. Everything built, the gauge beside it kept
// reporting a correct median, and no existing test noticed. That is the fourth time in this
// campaign that the counter which would have shown a defect was not live — the pattern #318
// and #319 were both about.
//
// It deliberately asserts LIVENESS and AGREEMENT, not a magnitude: the fake server answers in
// microseconds, and a bound cleared by a 27 µs fixture is the exact mistake that let #340's
// wrong-unit default ship as "verified live".
func TestEveryFillsFirstByteLatencyReachesTheRecorder(t *testing.T) {
	srv := fake.New()
	const objSize = 24 << 20
	body := make([]byte, objSize)
	for i := range body {
		body[i] = byte(i * 31 % 251)
	}
	srv.Put("obj", body, time.Unix(1_700_000_000, 0))

	rec := &ttfbRec{}
	// 1 MiB blocks so a 24 MiB walk is many fills rather than one, and the count below is a
	// real count.
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})
	k := keyFor(t, srv, "obj")

	// Before any fill: nothing observed, and MeasuredTTFB says so. The two must agree about
	// the unmeasured state, because lith_ttfb_measured is derived from the second and the
	// histogram from the first.
	if s, _ := rec.got(); len(s) != 0 {
		t.Fatalf("samples before any fill: %d", len(s))
	}
	if _, measured := bs.MeasuredTTFB(); measured {
		t.Fatal("MeasuredTTFB reports measured before any fill")
	}

	const readLen = int64(64) << 10
	for off := int64(0); off < objSize; off += readLen {
		b, err := bs.GetRange(context.Background(), k, off, readLen, objSize)
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		// Verify the bytes, so a fixture that silently serves nothing cannot make the
		// sample count below meaningless.
		if len(b) != int(readLen) || b[0] != byte(off*31%251) {
			t.Fatalf("read at %d returned %d bytes, first %d", off, len(b), b[0])
		}
	}

	samples, gets := rec.got()
	if len(samples) == 0 {
		t.Fatal("no first-byte latency reached the Recorder: the ttfbRecorder hook is not " +
			"wired into recordTTFB, so lith_ttfb_seconds would be an always-empty histogram")
	}
	// ONE SAMPLE PER FILL. Tier hits do not fill, so the count must track GETs and not reads:
	// 384 reads of 64 KiB over a 24 MiB object is 24 fills. An assertion of "> 0" would pass
	// on a single sample, and a hook that fired once per handle rather than once per fill is
	// precisely the shape of the per-handle-counter defect in #319.
	if int64(len(samples)) != gets {
		t.Errorf("observed %d first-byte latencies against %d GETs; the hook does not fire "+
			"once per fill", len(samples), gets)
	}
	if gets < 2 {
		t.Fatalf("fixture: %d GETs is not enough for the count above to mean anything", gets)
	}

	// AGREEMENT WITH THE GAUGE: the median the policy reads must be one of the samples the
	// histogram got. If these two drifted apart, a deployment reading the distribution to
	// explain the gate's behaviour would be reading a different quantity from the one the
	// gate decides on — which is what made #340 take a --timeline-csv to diagnose.
	med, measured := bs.MeasuredTTFB()
	if !measured {
		t.Fatal("MeasuredTTFB still reports unmeasured after a full walk")
	}
	found := false
	for _, s := range samples {
		if s == med {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the median the evidence policy reads (%v) is not among the %d samples the "+
			"recorder was handed", med, len(samples))
	}
	for _, s := range samples {
		if s <= 0 {
			t.Errorf("a non-positive latency %v was handed to the recorder; recordTTFB drops "+
				"those, so the histogram and the median would disagree on their sample count", s)
		}
	}

	// A Recorder that does NOT implement the extension must still work: the type assertion is
	// what keeps every other implementer (and every external one) from having to change.
	plain := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: &backRec{}})
	if _, err := plain.GetRange(context.Background(), keyFor(t, srv, "obj"), 0, readLen, objSize); err != nil {
		t.Fatalf("read with a Recorder lacking S3TTFB: %v", err)
	}
	if _, measured := plain.MeasuredTTFB(); !measured {
		t.Error("a Recorder without the extension suppressed the median too; the optional " +
			"interface is not optional")
	}
}
