// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"
	"time"
)

// #284/#292: the two inputs this must never guess from.
func TestEvidenceRatioFor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured float64
		ttfb       time.Duration
		measured   bool
		want       float64
	}{
		// AN EXPLICIT FLAG WINS, at every latency. An operator who set it has said what they
		// want; a latency heuristic does not get to overrule them, in either direction.
		{"flag wins in-region", 4, 2 * time.Millisecond, true, 4},
		{"flag wins cross-region", 4, 60 * time.Millisecond, true, 4},
		{"flag wins unmeasured", 4, 40 * time.Millisecond, false, 4},
		{"flag wins at an absurd latency", 8, 5 * time.Second, true, 8},

		// NO MEASUREMENT -> 0, never a derivation from the seed. That is #292: currentTTFB
		// falls back to a hard-coded 40 ms, so a derivation made before any fill produces the
		// same "device-derived" number on every endpoint on earth. #291 shipped that and
		// measured an identical 30-block floor at a 2.2 ms and a 58.6 ms endpoint.
		{"unmeasured is off", 0, 2 * time.Millisecond, false, 0},
		{"unmeasured is off even at the seed", 0, 40 * time.Millisecond, false, 0},
		{"unmeasured is off cross-region", 0, 60 * time.Millisecond, false, 0},

		// Measured, no flag: the policy decides. Near engages it, far does not; the
		// thresholds themselves are pinned in TestLatencyDerivedEvidenceRatio.
		{"measured in-region", 0, 2 * time.Millisecond, true, defaultEvidenceRatio},
		{"measured cross-region", 0, 58600 * time.Microsecond, true, 0},
		{"measured zero", 0, 0, true, 0},

		// A negative setting forces the gate off at any latency, since 0 now means
		// "decide for me".
		{"forced off, near", -1, 2 * time.Millisecond, true, 0},
		{"forced off, far", -1, 58600 * time.Microsecond, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceRatioFor(tc.configured, tc.ttfb, tc.measured); got != tc.want {
				t.Errorf("evidenceRatioFor(%v, %v, %v) = %v, want %v",
					tc.configured, tc.ttfb, tc.measured, got, tc.want)
			}
		})
	}
}

// THE POLICY, replacing the inertness test that guarded it (#284).
//
// That test said: "When Phase 2 turns the policy on, THIS TEST IS SUPPOSED TO FAIL. Replace it
// with the ladder's thresholds; do not weaken it." This is the replacement, and the thresholds
// are the measured ones rather than a ladder, because the evidence turned out to be two
// endpoints and nothing between them:
//
//	2.2 ms  : slice readers win on both axes; whole-object fast consumer +5% to +20%
//	58.6 ms : whole-object fast consumer +135% (r = 2.35, zero overlap)
//
// So the bound is conservative by construction. The cases below pin that it stays that way:
// the gate must not engage anywhere in the unmeasured middle, because the cost of being wrong
// there is 135% and the cost of being cautious is only a missed saving.
func TestLatencyDerivedEvidenceRatio(t *testing.T) {
	// THE VALUES HERE ARE FIRST-BYTE LATENCIES, NOT ROUND TRIPS, and that distinction is why
	// this test could not catch #340. The previous table used 2.2 ms and 58.6 ms -- the
	// measured RTTs -- against a bound that the policy applies to TTFB. Both numbers were
	// real and neither was the quantity under test, so the table passed while no in-region
	// mount could ever engage the gate.
	const (
		inRegionP10    = 22600 * time.Microsecond // measured, n=84
		inRegionMedian = 28200 * time.Microsecond
		inRegionP90    = 42700 * time.Microsecond
		crossRegionRTT = 58600 * time.Microsecond // TTFB cannot be below this at distance
	)
	for _, tc := range []struct {
		name string
		ttfb time.Duration
		want float64
	}{
		// ENGAGED across the whole measured in-region distribution. p90 is the one that
		// matters: a bound between the median and p90 would engage only sometimes.
		{"in-region p10", inRegionP10, defaultEvidenceRatio},
		{"in-region median", inRegionMedian, defaultEvidenceRatio},
		{"in-region p90", inRegionP90, defaultEvidenceRatio},
		{"just inside the bound", nearEndpointTTFB, defaultEvidenceRatio},

		// NOT ENGAGED at distance. TTFB includes a round trip, so a cross-region endpoint
		// cannot report below its RTT -- this side is excluded by construction, not by a
		// measurement, which is what makes the bound anchored rather than interpolated.
		{"cross-region RTT floor", crossRegionRTT, 0},
		{"cross-region plausible TTFB", 85 * time.Millisecond, 0},
		{"just past the bound", nearEndpointTTFB + time.Microsecond, 0},
		{"far", time.Second, 0},

		// A non-positive latency is not a measurement and must not engage by accident.
		{"zero", 0, 0},
		{"negative", -time.Millisecond, 0},

		// AND THE TRAP THAT HID #340: a fixture latency no real endpoint has. The fake
		// server returns in microseconds, so my "verified the gate is live" check passed
		// against 27us -- a value that clears any bound, including the broken one. Kept as a
		// case so the table says out loud that passing here is not evidence of anything.
		{"a fake server's microseconds, which prove nothing", 27 * time.Microsecond, defaultEvidenceRatio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := latencyDerivedEvidenceRatio(tc.ttfb); got != tc.want {
				t.Errorf("latencyDerivedEvidenceRatio(%v) = %v, want %v", tc.ttfb, got, tc.want)
			}
		})
	}

	// THE TWO ANCHORS, asserted as a relation rather than as literals, so a future change to
	// the bound has to stay inside them.
	if nearEndpointTTFB <= inRegionP90 {
		t.Errorf("bound %v is at or below in-region p90 %v: the gate would engage only "+
			"sometimes in-region, which is #340", nearEndpointTTFB, inRegionP90)
	}
	if nearEndpointTTFB >= crossRegionRTT {
		t.Errorf("bound %v is at or above the cross-region RTT %v: TTFB includes a round "+
			"trip, so the gate could engage at distance, which is the measured 2.35x "+
			"regression", nearEndpointTTFB, crossRegionRTT)
	}
}

// End to end through the decision function, which is what the read path calls: the gate now
// engages by DEFAULT on a near endpoint, and only there.
func TestEvidenceRatioEngagesByDefaultOnlyNearby(t *testing.T) {
	// Unconfigured and near: engaged. This is the behaviour change.
	if got := evidenceRatioFor(0, 28*time.Millisecond, true); got != defaultEvidenceRatio {
		t.Errorf("unconfigured at an in-region 28ms TTFB = %v, want %v (the gate must now default on "+
			"in-region)", got, defaultEvidenceRatio)
	}
	// Unconfigured and far: unchanged.
	if got := evidenceRatioFor(0, 85*time.Millisecond, true); got != 0 {
		t.Errorf("unconfigured at a cross-region 85ms TTFB = %v, want 0", got)
	}
	// Unconfigured and UNMEASURED: unchanged, whatever the seed says. A mount engages the gate
	// only once it has measured the endpoint itself (#292).
	if got := evidenceRatioFor(0, 28*time.Millisecond, false); got != 0 {
		t.Errorf("unconfigured and unmeasured = %v, want 0 — the gate engaged on the seed", got)
	}
	// A NEGATIVE setting forces it off, at any latency. 0 now means "decide for me", so there
	// has to be a way to say off and mean it.
	for _, d := range []time.Duration{time.Millisecond, 2 * time.Millisecond, time.Second} {
		if got := evidenceRatioFor(-1, d, true); got != 0 {
			t.Errorf("--readahead-evidence-ratio -1 at %v = %v, want 0 — an operator cannot "+
				"turn the gate off", d, got)
		}
	}
	// A positive setting still wins everywhere, including where the policy would say 0.
	if got := evidenceRatioFor(8, 58*time.Millisecond, true); got != 8 {
		t.Errorf("explicit 8 at 58ms = %v, want 8 — the policy overrode an operator", got)
	}
	if got := evidenceRatioFor(8, 2*time.Millisecond, true); got != 8 {
		t.Errorf("explicit 8 at 2ms = %v, want 8 — the policy overrode an operator", got)
	}
}
