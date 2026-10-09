// SPDX-License-Identifier: Apache-2.0

package main

import "fmt"

// pressureMaxFor decides the tier-pressure admission bound in force for a mount or an export:
// the fraction of the memory tier that outstanding prefetch commitment may reach before a
// dispatch is dropped (#313).
//
// WHAT IT IS FOR. The per-handle readahead window is rationed by dividing the prefetch budget
// by the live stream count, and at a CONCURRENT START that divisor lags the population -- N
// handles establish within milliseconds of each other while the divisor still reads 1, so each
// dispatches a full-budget window. The budget defaults to half the tier (blockstore.New:
// PrefetchBudget = MemCache/2), which is why steady-state pressure is 0.50 and why 16 readers
// starting together were measured at 8.000: N x 0.5. Everything above 1.0 must evict something
// unread to land.
//
// WHY ITS DEFAULT IS DERIVED FROM THE EVIDENCE RATIO AND NOT FROM THE REGION PAIR. The two
// gates bound the same quantity from opposite ends. The evidence gate bounds each handle by its
// OWN consumed bytes, which is available from its first read and therefore has no transient;
// this gate bounds the AGGREGATE against the tier, which does. Where the evidence gate is on,
// this one was measured never to bind -- peak pressure 0.50, zero holds -- so turning it on
// there would add a second ration with nothing to ration.
//
// Reading EvidenceRatioFor's result rather than re-deciding from nearRegion/regionKnown is
// deliberate. Two policies over the same booleans are two policies that can drift; one reading
// the other's answer cannot. It also composes correctly for cases no region pair describes:
//
//   - an operator who forces --readahead-evidence-ratio 4 on a cross-region mount has bounded
//     their windows by consumption, so this gate correctly stands down;
//   - an operator who forces --readahead-evidence-ratio -1 in-region has removed that bound,
//     so this gate correctly engages;
//   - `lith serve nfs` has NO evidence gate at all (internal/nfs carries its own minimal
//     detector and never calls into internal/prefetch), so it passes 0 and the gate engages
//     on every export. See the note on defaultPressureMax about what is and is not measured
//     there.
//
// Rules, in the same order as EvidenceRatioFor, because an operator who learns one knob's
// contract should not have to learn a second:
//
//  1. A POSITIVE --prefetch-pressure-max wins, always.
//  2. A NEGATIVE one forces the gate off. 0 means "decide for me", so there has to be a way to
//     say "off" and mean it -- including on a mount where the default would engage.
//  3. Evidence gate ON -> off. It does not bind there, and an inert ration is still a ration
//     an operator can trip over.
//  4. Evidence gate OFF -> defaultPressureMax.
func pressureMaxFor(configured, evidenceRatio float64) float64 {
	if configured > 0 {
		return configured
	}
	if configured < 0 {
		return 0
	}
	if evidenceRatio > 0 {
		return 0
	}
	return defaultPressureMax
}

// defaultPressureMax is 0.85, the low end of the measured band.
//
// THE BAND, MEASURED ON REAL S3 AGAINST AN UNBOUNDED START (peak pressure 8.000):
//
//	1.00 : wall 41 s against 116 s unbounded (2.8x), 71-119 unread chunks still evicted
//	0.85 : evictions 0-4, for about 10% more wall than 1.00
//	0.50 : over-throttled 55% against 1.00 (63 s vs 41 s) while buying nothing over 0.85
//	1.30 : held pressure at exactly 1.300 as asked and still evicted ~2100 -- see below
//
// 0.85 RATHER THAN 1.00, and the reason is a mechanism rather than a point between two
// observations. An evicted unread prefetch is a byte that was fetched and discarded. This
// default engages only where the evidence gate does not -- cross-region, region-unknown, or an
// export -- and cross-region those bytes are billed egress, not merely wasted request budget.
// So the trade is ~10% wall against ~100 discarded-byte events, on the exact mounts where a
// discarded byte has a price. In-region, where the wall cost would be the only term, the gate
// is off entirely.
//
// ABOVE 1.0 IS EXCLUDED BY ARITHMETIC, NOT BY MEASUREMENT: a threshold above 1.0 admits more
// unread bytes than the tier can hold, so the tier must evict one to take another -- the exact
// condition the gate exists to prevent. 1.3 was nevertheless proposed on this issue, fitted
// between a cell observed clean at 1.22 and one observed collapsed at 1.40, and the external
// cell refuted it by its own pre-registered criterion: the gate fired 94-100 times, bounded
// pressure exactly as designed, and evictions fell only ~2900 -> ~2100. It did its job
// perfectly and its job was impossible. pressureMaxWarning says so at the flag.
//
// WHAT IT DOES NOT DO, stated here because the number looks like a fix and is not one: it
// bounds BYTES, NOT FAIRNESS. Reader spread stayed 2.3-3.1 at every threshold tested, against
// ~1.05 with the evidence gate on. It limits the collapse; the fairness half of the gate-off
// case is still open.
//
// THE POPULATION THIS WAS MEASURED ON is a FUSE mount with the evidence gate off, 16 concurrent
// readers, a 33 GB box. `lith serve nfs` inherits it by the arithmetic -- the gate lives in the
// blockstore, reads the same committed-bytes counter against the same tier, and the gateway has
// no consumed-bytes bound of any kind -- but NOT by its own measurement. #337's gateway cell is
// where that gets checked; until then the gateway is bounded by a figure derived elsewhere,
// which is better than unbounded and worth saying out loud.
const defaultPressureMax = 0.85

// pressureBudgetWarning returns a warning when the prefetch budget is large enough that the
// pressure gate will bind in STEADY STATE rather than only on the start transient, and "" when
// it will not (#313).
//
// The gate exists to catch a concurrent-start overshoot. It is sized against the tier, while
// the quantity it actually limits -- outstanding un-demanded prefetch -- is independently
// bounded by --prefetch-budget, which DEFAULTS TO HALF THE TIER. That is the whole reason the
// default works: one handle's steady-state pressure is ~0.50, comfortably under 0.85, so the
// gate is silent until the divisor lags and several handles commit a full budget each.
//
// Raise --prefetch-budget above pressureMax x tier and that stops being true: a single handle
// at its full budget now sits at or above the threshold, so the gate holds dispatches
// continuously and the mount throttles itself with nothing wrong. Two knobs that are
// individually reasonable compose into a self-inflicted stall, and nothing else in the mount
// would say so -- lith_prefetch_pressure_held_total would climb and look like the gate working.
func pressureBudgetWarning(pressureMax float64, prefetchBudget, tierBytes int64) string {
	if pressureMax <= 0 || prefetchBudget <= 0 || tierBytes <= 0 {
		return ""
	}
	bind := int64(pressureMax * float64(tierBytes))
	if prefetchBudget < bind {
		return ""
	}
	return fmt.Sprintf("--prefetch-budget %d bytes is %.0f%% of the %d-byte memory tier, at or "+
		"above the --prefetch-pressure-max threshold of %g (%d bytes), so the pressure gate "+
		"will hold dispatches in STEADY STATE and not just on a concurrent start — the mount "+
		"will throttle itself. Lower --prefetch-budget below %d bytes, raise --mem-cache, or "+
		"pass --prefetch-pressure-max -1 to turn the gate off deliberately (#313)",
		prefetchBudget, 100*float64(prefetchBudget)/float64(tierBytes), tierBytes,
		pressureMax, bind, bind)
}
