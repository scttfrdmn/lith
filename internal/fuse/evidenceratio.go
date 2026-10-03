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
//  1. An explicit --readahead-evidence-ratio wins, always. An operator who set it has said
//     what they want and is not second-guessed by a latency heuristic.
//  2. No measurement yet -> 0, today's behaviour. NEVER derive from the seed: that is #292,
//     where a 40 ms constant masquerades as a device measurement on every endpoint.
//  3. Otherwise the latency-derived ratio.
//
// Rule 3 currently returns 0 for every input, so this is inert by construction. The constants
// need a TTFB ladder between the two anchor points above, and this campaign has already
// refuted five mechanisms and shipped one 11x regression whose justifying argument agreed with
// the thing it replaced to within 2.4%. The policy lands when it is measured, not when it is
// plausible.
func evidenceRatioFor(configured float64, ttfb time.Duration, measured bool) float64 {
	if configured > 0 {
		return configured
	}
	if !measured {
		return 0
	}
	return latencyDerivedEvidenceRatio(ttfb)
}

// latencyDerivedEvidenceRatio is the policy proper: the gate ratio for an endpoint whose
// first-byte latency has been MEASURED at ttfb.
//
// Deliberately 0 everywhere pending Phase 2's ladder. Kept as its own function so the policy
// has one place to live and one place to be tested, and so turning it on is a change to this
// body alone rather than to the plumbing.
func latencyDerivedEvidenceRatio(ttfb time.Duration) float64 {
	_ = ttfb
	return 0
}
