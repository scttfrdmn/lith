// SPDX-License-Identifier: Apache-2.0

package fuse

import "time"

// evidenceRatioFor decides the #256 evidence-gate ratio in force for a read: the bound on a
// committed readahead window as a multiple of the bytes the handle has actually consumed.
//
// WHY THIS IS A FUNCTION OF LATENCY. The gate exists because the window is sized by
// concurrency and by the NIC, never by how much of the object a handle is going to want. A
// single process reading one variable of a multi-variable NetCDF-4 file was measured fetching
// the WHOLE object -- 54.03x over-fetch, equal to 1/coverage to within 0.4% on two different
// objects (#284). The access is perfectly sequential within the variable, which is the point:
// sequential is what earns the full window, so the most sequential low-coverage reader pays
// most. `--readahead-evidence-ratio 4` fixes it.
//
// It is off by default because its cost is RTT-scaled, and sharply:
//
//	in-region, ~2.2 ms : byte-identical, wall indistinguishable, over-fetch cut 56-95%
//	cross-region, 58.6 ms : 2.56x the wall, zero overlap, 64/64 pairs, p = 1.6e-4
//
// (bench/evidence-ratio/ and bench/evidence-ratio/high-rtt/.) fetchExtents issues a window's
// blocks in ONE call, so the window at establishment is the batch size; capping it at distance
// costs a round trip per batch that nothing later recovers.
//
// So the quantity that decides whether the gate is free is one lith already samples. Rules, in
// order:
//
//  1. A POSITIVE --readahead-evidence-ratio wins, always. An operator who set it has said what
//     they want and is not second-guessed by a latency heuristic.
//  2. A NEGATIVE one forces the gate off. 0 now means "decide for me", so there has to be a
//     way to say "off" and mean it.
//  3. No measurement yet -> 0, the pre-#284 behaviour. NEVER derive from the seed: that is
//     #292, where a 40 ms constant masquerades as a device measurement on every endpoint. A
//     mount engages the gate only once it has measured that the endpoint is near.
//  4. Otherwise the latency-derived ratio.
func evidenceRatioFor(configured float64, ttfb time.Duration, measured bool) float64 {
	if configured > 0 {
		return configured
	}
	if configured < 0 {
		return 0
	}
	if !measured {
		return 0
	}
	return latencyDerivedEvidenceRatio(ttfb)
}

// defaultEvidenceRatio and nearEndpointTTFB are the measured policy (#284, corrected in #340).
//
// The ratio is 4 because every cell that decided this used 4; no other value has been
// measured on any shape.
//
// THE BOUND IS IN TTFB, AND v1.4.0 SHIPPED IT IN RTT. That is the whole of #340. The policy
// reads BlockStore.MeasuredTTFB, which is the rolling median of S3 FIRST-BYTE latency per
// fill. The original 5 ms was justified as "a little over twice the measured-good point" where
// that point was 2.2 ms -- the network ROUND TRIP. In-region first-byte latency is 28.2 ms
// median (p10 22.6, p90 42.7, n=84), so the bound sat 4.5x below p10 and NO in-region mount
// ever engaged: an external deployment measured the default reproducing #284's 54.02x
// over-fetch byte for byte, with the gauge reading 0 after 162 completed fills.
//
// It is the same unit mix-up as #329, where 2.2 ms was used as a 1 MiB GET's unit price. That
// one was caught in an argument; this one shipped as a constant.
//
// 50 ms IS ANCHORED ON BOTH SIDES, so it is not a guess in an unmeasured gap:
//
//	lower bound, MEASURED: in-region TTFB p90 is 42.7 ms, so 50 clears the whole
//	                       in-region distribution and the median of eight samples
//	                       the policy actually reads is nowhere near it.
//	upper bound, BY CONSTRUCTION: TTFB includes at least one round trip, so a
//	                       cross-region endpoint at 58.6 ms RTT cannot report a TTFB
//	                       below 58.6 ms. No measurement is needed to exclude it.
//
// Erring low is also the safe direction: too low and the gate never engages, which is the
// pre-#330 behaviour and costs only the saving. Too high and it engages at distance, which is
// the measured 2.35x regression on a whole-object fast consumer.
const (
	defaultEvidenceRatio = 4.0
	// nearEndpointTTFB is a FIRST-BYTE latency, not a round trip. See above.
	nearEndpointTTFB = 50 * time.Millisecond
)

// latencyDerivedEvidenceRatio is the policy proper: the gate ratio for an endpoint whose
// first-byte latency has been MEASURED at ttfb.
//
// Kept as its own function so the policy has one place to live and one place to be tested.
//
// WHAT THE GATE BUYS where it engages, all measured on real S3: a single process reading one
// variable of a multi-variable NetCDF-4 file goes from fetching the WHOLE object to 2.65x of
// what it wanted -- 54x over-fetch down to 2.65x, and 20.4x fewer bytes cross-region where it
// also wins wall clock (r = 0.84). What it costs is a fixed ~0.07-0.18 s on the one shape that
// pays, a fast consumer reading most of an object: r = 1.047 to 1.206 in-region across a 14.5x
// size range and two boxes, bounded because the baseline carries its own fixed cost.
func latencyDerivedEvidenceRatio(ttfb time.Duration) float64 {
	if ttfb > 0 && ttfb <= nearEndpointTTFB {
		return defaultEvidenceRatio
	}
	return 0
}
