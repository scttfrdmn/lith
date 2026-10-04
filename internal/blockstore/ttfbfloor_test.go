// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"testing"
	"time"
)

// #349: the floor is LOAD-INVARIANT where the median is not.
//
// The median the evidence policy reads moves with lith's own in-flight depth: measured
// in-region, ~28 ms on an idle mount and ~100 ms during that same mount's prefetch burst,
// which is above both the 50 ms bound and the 58.6 ms cross-region round trip the bound's far
// side is anchored on. A busy near endpoint and an idle far one are not separable on it.
//
// The floor can make a claim the median cannot: queueing and contention only ADD to a
// first-byte latency, so the low end of a window is a lower bound on what the endpoint costs.
// This test is that claim, as the two distributions that have to come apart.
func TestFloorTTFBSeparatesALoadedNearEndpointFromAnIdleFarOne(t *testing.T) {
	// The measured in-region shape (#340/#349), and PHASE ORDER is the mechanism, not just
	// the mix: MeasuredTTFB is the median of the last EIGHT samples, so what matters is that
	// each open is ~16 serial demand GETs in the 20s followed by a burst tail near 100 ms.
	// The fast samples arrive first and fall out of the 8-window before the read ends, which
	// is why the end-of-read median is ~100 ms even though two thirds of the open's samples
	// were under 50 ms (16 of 23, six consecutive opens, on the issue). An interleaved
	// fixture of the same proportions does NOT reproduce it -- the first draft of this test
	// made exactly that mistake and its own precondition caught it.
	nearLoaded := make([]time.Duration, 0, 138)
	for open := 0; open < 6; open++ {
		for i := 0; i < 16; i++ { // blocks 0-1: serial demand GETs, unqueued
			nearLoaded = append(nearLoaded, time.Duration(22+i%12)*time.Millisecond)
		}
		for i := 0; i < 7; i++ { // the handle's own prefetch burst
			nearLoaded = append(nearLoaded, time.Duration(95+i*2)*time.Millisecond)
		}
	}
	// A cross-region endpoint, idle. Its floor cannot go under its round trip -- a first
	// byte does not arrive sooner than one -- so every sample is at or above 58.6 ms.
	farIdle := make([]time.Duration, 0, 138)
	for i := 0; i < 138; i++ {
		farIdle = append(farIdle, time.Duration(59+i%31)*time.Millisecond)
	}

	feed := func(samples []time.Duration) *BlockStore {
		bs := &BlockStore{ttfbSeed: 40 * time.Millisecond}
		for _, d := range samples {
			bs.recordTTFB(d)
		}
		return bs
	}

	near, far := feed(nearLoaded), feed(farIdle)

	nearMed, ok := near.MeasuredTTFB()
	if !ok {
		t.Fatal("near: no median after 128 fills")
	}
	farMed, ok := far.MeasuredTTFB()
	if !ok {
		t.Fatal("far: no median after 128 fills")
	}
	nearFloor, ok := near.FloorTTFB()
	if !ok {
		t.Fatal("near: no floor after 128 fills")
	}
	farFloor, ok := far.FloorTTFB()
	if !ok {
		t.Fatal("far: no floor after 128 fills")
	}
	t.Logf("near-loaded: median %v floor %v | far-idle: median %v floor %v",
		nearMed, nearFloor, farMed, farFloor)

	// THE PREMISE, asserted so this test cannot pass for the wrong reason: the medians must
	// NOT separate these two across the 50 ms bound. If the fixture's medians already fall
	// on opposite sides, the floor is solving a problem the fixture does not pose.
	const bound = 50 * time.Millisecond
	if nearMed <= bound {
		t.Fatalf("fixture: the loaded near endpoint's median is %v, inside the bound — the "+
			"ambiguity #349 reports is not present", nearMed)
	}
	if farMed <= bound {
		t.Fatalf("fixture: the idle far endpoint's median is %v, inside the bound", farMed)
	}

	// THE CLAIM: the floors separate, and on the correct sides.
	if nearFloor > bound {
		t.Errorf("the loaded near endpoint's floor is %v, above the %v bound: the floor is "+
			"not load-invariant and #349's fix does not work", nearFloor, bound)
	}
	if farFloor <= bound {
		t.Errorf("the idle far endpoint's floor is %v, inside the %v bound: a far endpoint "+
			"would engage the gate, which is the 2.35x regression the bound exists to avoid",
			farFloor, bound)
	}
	// And with margin, not by a hair -- a separation of a millisecond or two would be a
	// fixture artifact rather than a property.
	if farFloor < 2*nearFloor {
		t.Errorf("floors %v and %v are within 2x; the separation is too thin to place a "+
			"bound between", nearFloor, farFloor)
	}

	// A low quantile, not the minimum: one anomalously fast sample must not define the
	// endpoint. Inject one well below the far endpoint's round trip and the floor must
	// barely move.
	before, _ := far.FloorTTFB()
	far.recordTTFB(1 * time.Millisecond)
	after, _ := far.FloorTTFB()
	if after < before-time.Millisecond {
		t.Errorf("one 1ms sample moved the far floor from %v to %v; the estimator is a "+
			"minimum in disguise", before, after)
	}
}

// Not-measured is reported, never estimated from a handful of fills (#292's rule).
func TestFloorTTFBWithholdsAnEstimateUntilItHasSamples(t *testing.T) {
	bs := &BlockStore{ttfbSeed: 40 * time.Millisecond}
	if d, ok := bs.FloorTTFB(); ok {
		t.Errorf("a floor of %v was reported with no fills at all", d)
	}
	// The median is available from the FIRST fill, because the policy that reads it needs an
	// answer early. The floor is a different contract: it is an estimate of a distribution,
	// so it withholds until it has one.
	bs.recordTTFB(28 * time.Millisecond)
	if _, ok := bs.MeasuredTTFB(); !ok {
		t.Error("the median is not available after one fill")
	}
	if _, ok := bs.FloorTTFB(); ok {
		t.Error("a floor was reported from one fill")
	}
	for i := 1; i < ttfbFloorMin; i++ {
		bs.recordTTFB(28 * time.Millisecond)
	}
	if _, ok := bs.FloorTTFB(); !ok {
		t.Errorf("no floor after %d fills, the stated minimum", ttfbFloorMin)
	}

	// The long window must outlive the policy's 8-sample one: that is the entire point.
	// After ttfbMax fast fills the median is fast, while the floor still remembers the
	// slow ones -- and vice versa, which is the #349 shape.
	for i := 0; i < 200; i++ {
		bs.recordTTFB(110 * time.Millisecond)
	}
	med, _ := bs.MeasuredTTFB()
	floor, _ := bs.FloorTTFB()
	if med < 100*time.Millisecond {
		t.Errorf("median %v after 200 slow fills; the 8-window did not turn over", med)
	}
	if floor > 50*time.Millisecond {
		t.Errorf("floor %v after 200 slow fills: the long window is not retaining the fast "+
			"samples that make it a floor", floor)
	}
}

// THIS TEST IS SUPPOSED TO FAIL when the policy moves onto the floor (#349).
//
// Same role as TestEvidenceRatioPolicyIsInertUntilMeasured had for #322's phase 1: the floor
// is an INSTRUMENT right now, added so the deciding measurement can be taken before anything
// is gated on it. If a change makes the policy read it, this test is the notice that the
// change was intentional -- delete it then, and say which measurement justified it.
func TestFloorTTFBIsAnInstrumentAndNothingReadsIt(t *testing.T) {
	bs := &BlockStore{ttfbSeed: 40 * time.Millisecond}
	// A distribution whose floor and median fall on OPPOSITE sides of the 50 ms bound, so
	// a policy reading one would behave differently from a policy reading the other.
	for i := 0; i < 128; i++ {
		if i%4 == 0 {
			bs.recordTTFB(24 * time.Millisecond)
		} else {
			bs.recordTTFB(105 * time.Millisecond)
		}
	}
	med, _ := bs.MeasuredTTFB()
	floor, ok := bs.FloorTTFB()
	if !ok {
		t.Fatal("no floor")
	}
	if med <= 50*time.Millisecond || floor > 50*time.Millisecond {
		t.Fatalf("fixture: median %v and floor %v must straddle the bound for this to "+
			"detect anything", med, floor)
	}
	// MeasuredTTFB is what evidenceRatioFor is handed, and it must still be the median.
	if med == floor {
		t.Error("MeasuredTTFB and FloorTTFB returned the same value; the policy's input " +
			"has been changed without this test being updated")
	}
}
