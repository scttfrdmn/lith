// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"testing"
	"time"
)

// #292: currentTTFB falls back to a hard-coded 40 ms seed, so any consumer reading it before a
// fill has completed receives a plausible constant that is identical on every endpoint. #291
// shipped a readahead floor derived that way and measured it at exactly 30 blocks on both a
// 2.2 ms and a 58.6 ms endpoint; the 58.6 ms round trip wants 44.
//
// MeasuredTTFB returns the second value so a caller cannot take the seed by accident.
func TestMeasuredTTFBDistinguishesTheSeedFromAMeasurement(t *testing.T) {
	bs := &BlockStore{ttfbSeed: 40 * time.Millisecond}

	d, measured := bs.MeasuredTTFB()
	if measured {
		t.Error("a fresh store reports its seed as a measurement")
	}
	if d != 40*time.Millisecond {
		t.Errorf("seed = %v, want 40ms", d)
	}
	// currentTTFB keeps its old contract for the callers that knowingly want the fallback.
	if got := bs.currentTTFB(); got != 40*time.Millisecond {
		t.Errorf("currentTTFB on a fresh store = %v, want the seed", got)
	}

	// One real sample flips it, and the value is the sample rather than the seed.
	bs.recordTTFB(3 * time.Millisecond)
	d, measured = bs.MeasuredTTFB()
	if !measured {
		t.Error("a store with a sample still reports not-measured")
	}
	if d != 3*time.Millisecond {
		t.Errorf("one sample: %v, want 3ms", d)
	}

	// The median, not the latest or the mean — so one slow fill cannot move the policy.
	for _, s := range []time.Duration{2, 4, 3, 900, 3} {
		bs.recordTTFB(s * time.Millisecond)
	}
	d, measured = bs.MeasuredTTFB()
	if !measured {
		t.Fatal("not measured after six samples")
	}
	if d > 10*time.Millisecond {
		t.Errorf("median = %v: a single 900ms outlier moved it, so it is not a median", d)
	}

	// A non-positive sample is ignored rather than recorded as zero, which would drag the
	// median toward a latency no endpoint has.
	before, _ := bs.MeasuredTTFB()
	bs.recordTTFB(0)
	bs.recordTTFB(-time.Second)
	if after, _ := bs.MeasuredTTFB(); after != before {
		t.Errorf("a zero/negative sample changed the median from %v to %v", before, after)
	}
}
