// SPDX-License-Identifier: Apache-2.0

package fuse

import "testing"

// The policy, as a table. Each row is a decision an operator or a deployment can land in.
func TestEvidenceRatioFor(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		configured             float64
		nearRegion, regionKnwn bool
		want                   float64
	}{
		// An operator who set it is not second-guessed, in either direction.
		{"positive flag wins in-region", 2.5, true, true, 2.5},
		{"positive flag wins cross-region", 2.5, false, true, 2.5},
		{"positive flag wins with no region at all", 2.5, false, false, 2.5},
		// Negative forces off. 0 means "decide for me", so there has to be a way to say
		// "off" and mean it -- including on the mount where the default would engage.
		{"negative forces off in-region", -1, true, true, 0},
		{"negative forces off cross-region", -1, false, true, 0},

		// THE POLICY PROPER.
		{"same region engages", 0, true, true, defaultEvidenceRatio},
		{"different region does not", 0, false, true, 0},
		// UNKNOWN IS OFF, not "assume near". IMDS is blocked on plenty of hardened images
		// and a custom --endpoint has no AWS region at all. Erring off costs only the
		// saving; erring on is the measured ~2x at distance (r = 1.96, zero overlap).
		{"unknown region is off", 0, false, false, 0},
		// And specifically: a true nearRegion with regionKnown false must NOT engage. That
		// combination is what a caller produces by forgetting to set the second field, and
		// defaulting it on would turn a plumbing slip into a 2x regression at distance.
		{"near but not known is off", 0, true, false, 0},
	} {
		got := evidenceRatioFor(tc.configured, tc.nearRegion, tc.regionKnwn)
		if got != tc.want {
			t.Errorf("%s: evidenceRatioFor(%v, near=%v, known=%v) = %v, want %v",
				tc.name, tc.configured, tc.nearRegion, tc.regionKnwn, got, tc.want)
		}
	}
}

// THE PROPERTY THE OLD POLICY COULD NOT HAVE: the gate's state is a function of
// configuration, so it is the same on every read of a mount's life (#349).
//
// The latency-derived policy it replaced was measured producing 9.1x, 5.1x and 1.05x
// over-fetch on three IDENTICAL cells, with the ratio gauge reading on for 50-81% of ticks
// depending on the run, because its input moved with the load the gate's own decision
// created. "Amplification is reproducible from configuration" is the property a default has
// to have, and it is the one that was missing.
func TestEvidenceRatioIsStableAcrossAMountsLife(t *testing.T) {
	for _, cfg := range []struct {
		name                   string
		configured             float64
		nearRegion, regionKnwn bool
	}{
		{"in-region default", 0, true, true},
		{"cross-region default", 0, false, true},
		{"unknown endpoint", 0, false, false},
		{"forced on", 4, false, true},
		{"forced off", -1, true, true},
	} {
		first := evidenceRatioFor(cfg.configured, cfg.nearRegion, cfg.regionKnwn)
		// The same inputs 1000 reads later. Nothing the mount does to itself -- burst
		// depth, queueing, eviction -- can appear in these arguments, which is the whole
		// point of the change: there is no run-dependent term to vary.
		for range 1000 {
			if got := evidenceRatioFor(cfg.configured, cfg.nearRegion, cfg.regionKnwn); got != first {
				t.Fatalf("%s: ratio moved from %v to %v with identical inputs",
					cfg.name, first, got)
			}
		}
	}
}

// The two measured arms, as the policy sees them. This is the split the region pair exists to
// encode, and the numbers are from the cells that decided it.
func TestEvidenceRatioEncodesTheMeasuredSplit(t *testing.T) {
	// In-region: forcing the gate on took six concurrent slice readers from 3281 / 1827 /
	// 375 MB (three identical cells) to 358.6 MB in 3/3, r = 1.00, with no wall-clock cost.
	// So the default must engage here.
	if got := evidenceRatioFor(0, true, true); got != defaultEvidenceRatio {
		t.Errorf("in-region default = %v, want %v: the 5-9x concurrent over-fetch is not "+
			"fixed", got, defaultEvidenceRatio)
	}
	// Cross-region: forcing it on cost r = 1.96 on a dd whole-object read with ZERO overlap
	// (min forced 15.80 s > max off 12.07 s, n = 4, p = 1/70). So the default must not.
	if got := evidenceRatioFor(0, false, true); got != 0 {
		t.Errorf("cross-region default = %v, want 0: this is the measured ~2x ramp penalty "+
			"at distance", got)
	}
	// And the ratio itself is 4 because every cell that decided it used 4. A change here is
	// a change to an unmeasured value, which is how two bad defaults shipped already.
	if defaultEvidenceRatio != 4.0 {
		t.Errorf("defaultEvidenceRatio = %v; no other value has been measured on any shape",
			defaultEvidenceRatio)
	}
}
