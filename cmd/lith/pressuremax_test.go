// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	fusefs "github.com/scttfrdmn/lith/internal/fuse"
)

// The policy, as a table. Each row is a decision an operator or a deployment can land in.
func TestPressureMaxFor(t *testing.T) {
	for _, tc := range []struct {
		name          string
		configured    float64
		evidenceRatio float64
		want          float64
	}{
		// An operator who set it is not second-guessed, in either direction, and the
		// evidence gate's state does not override them.
		{"positive flag wins with the evidence gate off", 0.9, 0, 0.9},
		{"positive flag wins with the evidence gate on", 0.9, 4, 0.9},
		// Negative forces off. 0 means "decide for me", so there has to be a way to say
		// "off" and mean it -- including where the default would engage.
		{"negative forces off with the evidence gate off", -1, 0, 0},
		{"negative forces off with the evidence gate on", -1, 4, 0},

		// THE POLICY PROPER: this gate covers exactly the mounts the evidence gate does not.
		{"evidence gate on stands down", 0, 4, 0},
		{"evidence gate off engages the default", 0, 0, defaultPressureMax},
	} {
		got := pressureMaxFor(tc.configured, tc.evidenceRatio)
		if got != tc.want {
			t.Errorf("%s: pressureMaxFor(%v, evidence=%v) = %v, want %v",
				tc.name, tc.configured, tc.evidenceRatio, got, tc.want)
		}
	}
}

// THE INVARIANT THE WHOLE DESIGN RESTS ON: at stock defaults, EXACTLY ONE of the two gates is
// in force on any mount, for every region pair. Neither both (a second ration with nothing to
// ration, and a wall-clock cost for it) nor neither (the unbounded concurrent start that was
// measured at peak pressure 8.000 and 116 s against 41 s).
//
// This is asserted over the real EvidenceRatioFor rather than a copy of its rules, so the day
// someone changes that policy's region split this test decides whether the pressure default
// still complements it. Two policies over the same booleans could drift silently; this is the
// thing that notices.
func TestExactlyOneAdmissionGateIsInForceAtDefaults(t *testing.T) {
	for _, rp := range []struct {
		name                   string
		nearRegion, regionKnwn bool
	}{
		{"in-region", true, true},
		{"cross-region", false, true},
		{"region unknown (off-EC2, IMDS blocked, custom endpoint)", false, false},
		{"near but not known", true, false},
	} {
		evidence := fusefs.EvidenceRatioFor(0, rp.nearRegion, rp.regionKnwn)
		pressure := pressureMaxFor(0, evidence)
		switch {
		case evidence > 0 && pressure > 0:
			t.Errorf("%s: BOTH gates in force (evidence %v, pressure %v) — the pressure gate "+
				"was measured never to bind where the evidence gate is on (peak 0.50, zero "+
				"holds), so this is a ration with nothing to ration", rp.name, evidence, pressure)
		case evidence <= 0 && pressure <= 0:
			t.Errorf("%s: NEITHER gate in force — nothing bounds a concurrent start, which is "+
				"the 8.000-pressure / 116 s case #313 is about", rp.name)
		}
	}
}

// The composition cases no region pair describes, named in pressureMaxFor's doc comment. Each
// is an operator overriding one gate, where the other must react rather than stay put.
func TestPressureDefaultComposesWithAForcedEvidenceRatio(t *testing.T) {
	// Forced ON at distance: the operator has bounded their windows by consumption, so this
	// gate has nothing left to do and must stand down.
	if got := pressureMaxFor(0, fusefs.EvidenceRatioFor(4, false, true)); got != 0 {
		t.Errorf("--readahead-evidence-ratio 4 cross-region: pressure = %v, want 0 — the "+
			"consumption bound is in force, so a second ration only costs wall clock", got)
	}
	// Forced OFF in-region: the operator has REMOVED the consumption bound, so the mount is
	// now the unbounded-start case even though it is near, and this gate must engage.
	if got := pressureMaxFor(0, fusefs.EvidenceRatioFor(-1, true, true)); got != defaultPressureMax {
		t.Errorf("--readahead-evidence-ratio -1 in-region: pressure = %v, want %v — nothing "+
			"else bounds the start once the evidence gate is forced off", got, defaultPressureMax)
	}
}

// The gateway's case, which is a literal 0 rather than a region pair, because internal/nfs
// never calls into internal/prefetch and so has no consumption bound at any region pair.
func TestTheGatewayAlwaysGetsTheGate(t *testing.T) {
	if got := pressureMaxFor(0, 0); got != defaultPressureMax {
		t.Errorf("serve nfs default = %v, want %v: an export's only admission bound", got,
			defaultPressureMax)
	}
	if got := pressureMaxFor(-1, 0); got != 0 {
		t.Errorf("serve nfs with -1 = %v, want 0: an operator can still turn it off", got)
	}
}

// The state is a function of configuration, so it is the same on every read of a mount's life.
// This is the property #349 proved a latency-derived policy cannot have -- the same workload
// came out at 9.1x, 5.1x and 1.05x over-fetch on three IDENTICAL cells because the input moved
// with the load the decision created. There is no run-dependent term in these arguments, which
// is the point.
func TestPressureMaxIsStableAcrossAMountsLife(t *testing.T) {
	for _, cfg := range [][2]float64{{0, 0}, {0, 4}, {0.9, 0}, {-1, 0}, {-1, 4}} {
		first := pressureMaxFor(cfg[0], cfg[1])
		for range 1000 {
			if got := pressureMaxFor(cfg[0], cfg[1]); got != first {
				t.Fatalf("pressureMaxFor%v moved from %v to %v with identical inputs",
					cfg, first, got)
			}
		}
	}
}

// The default must sit inside the band its own warning function considers usable. These two are
// independently editable and both encode the same measurement, so a change to one without the
// other ships a default that warns about itself.
func TestTheDefaultIsInsideTheMeasuredBand(t *testing.T) {
	if defaultPressureMax != 0.85 {
		t.Errorf("defaultPressureMax = %v, want 0.85: the only other measured values are 1.0 "+
			"(71-119 evictions left), 0.5 (over-throttled 55%%) and 1.3 (cannot work by "+
			"arithmetic)", defaultPressureMax)
	}
	if w := pressureMaxWarning(defaultPressureMax); w != "" {
		t.Errorf("the shipping default warns about itself: %q", w)
	}
	// And specifically: above 1.0 is excluded by arithmetic, so the default must never be
	// there however the band is re-measured.
	if defaultPressureMax > 1.0 {
		t.Errorf("defaultPressureMax = %v admits more unread bytes than the tier can hold, so "+
			"the tier still evicts to take them — the condition the gate exists to prevent",
			defaultPressureMax)
	}
}

// The one way these two knobs compose into a self-inflicted stall, asserted at the exact
// threshold rather than approximately.
func TestPressureBudgetWarning(t *testing.T) {
	const tier = int64(8) << 30
	// THE SHIPPING GEOMETRY MUST BE SILENT. --prefetch-budget defaults to MemCache/2, so a
	// single handle at its full budget sits at pressure ~0.50 against a 0.85 threshold. This
	// is why the default works at all, and it is the row that would fail if either default
	// moved toward the other.
	if w := pressureBudgetWarning(defaultPressureMax, tier/2, tier); w != "" {
		t.Errorf("the shipping default geometry warns (budget = tier/2 = pressure 0.50 against "+
			"a %v threshold): %q", defaultPressureMax, w)
	}
	// At the threshold it binds, and one byte below it does not. The gate holds on
	// `pressure >= max`, so the boundary is inclusive and the warning's must be too.
	pm := float64(defaultPressureMax) // a variable, so this is the same multiply the code does
	bind := int64(pm * float64(tier))
	if w := pressureBudgetWarning(defaultPressureMax, bind, tier); w == "" {
		t.Errorf("budget %d = exactly %v of the tier: no warning, but the gate holds on >= so "+
			"a single handle at full budget stalls the mount", bind, defaultPressureMax)
	}
	if w := pressureBudgetWarning(defaultPressureMax, bind-1, tier); w != "" {
		t.Errorf("budget one byte under the threshold warns: %q", w)
	}
	// Well above: still one warning, and it must name the number to lower.
	w := pressureBudgetWarning(defaultPressureMax, tier, tier)
	if w == "" {
		t.Fatal("a budget equal to the whole tier does not warn")
	}
	// A gate that is off cannot self-throttle, and neither can a mount with no memory tier
	// (prefetchPressure returns 0 when memCap is 0, so the gate is inert — a disk-only mount
	// must not be warned about a stall it cannot have).
	for _, tc := range []struct {
		name              string
		max               float64
		budget, tierBytes int64
	}{
		{"gate off by flag", 0, tier, tier},
		{"gate forced off", -1, tier, tier},
		{"no memory tier", defaultPressureMax, tier, 0},
		{"no budget", defaultPressureMax, 0, tier},
	} {
		if w := pressureBudgetWarning(tc.max, tc.budget, tc.tierBytes); w != "" {
			t.Errorf("%s: warned when it cannot bind: %q", tc.name, w)
		}
	}
}
