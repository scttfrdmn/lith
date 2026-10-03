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

		// Measured, no flag: the policy. Inert in this phase — see below.
		{"measured in-region", 0, 2 * time.Millisecond, true, 0},
		{"measured cross-region", 0, 58600 * time.Microsecond, true, 0},
		{"measured zero", 0, 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceRatioFor(tc.configured, tc.ttfb, tc.measured); got != tc.want {
				t.Errorf("evidenceRatioFor(%v, %v, %v) = %v, want %v",
					tc.configured, tc.ttfb, tc.measured, got, tc.want)
			}
		})
	}
}

// THIS PHASE MUST BE INERT, and that is worth asserting rather than asserting by inspection.
//
// The plumbing landed before the policy on purpose: the constants need a TTFB ladder between
// the two measured anchor points (in-region, where the gate is byte-identical and free;
// 58.6 ms, where it costs a clean 2.56x), and this campaign has already shipped one 11x
// regression on a mechanism whose justifying argument looked sound. So until the ladder exists,
// an unconfigured mount must behave exactly as it did — whatever the endpoint measures.
//
// When Phase 2 turns the policy on, THIS TEST IS SUPPOSED TO FAIL. Replace it with the ladder's
// thresholds; do not weaken it.
func TestEvidenceRatioPolicyIsInertUntilMeasured(t *testing.T) {
	for _, d := range []time.Duration{
		0, time.Microsecond, time.Millisecond, 2 * time.Millisecond,
		10 * time.Millisecond, 40 * time.Millisecond, 58600 * time.Microsecond,
		200 * time.Millisecond, time.Second, time.Hour,
	} {
		if got := latencyDerivedEvidenceRatio(d); got != 0 {
			t.Errorf("latencyDerivedEvidenceRatio(%v) = %v, want 0 — the policy is not yet "+
				"measured, so an unconfigured mount must not change behaviour. If you are "+
				"landing Phase 2, replace this test with the ladder's thresholds.", d, got)
		}
		if got := evidenceRatioFor(0, d, true); got != 0 {
			t.Errorf("an unconfigured mount at a measured %v got ratio %v, want 0", d, got)
		}
	}
}
