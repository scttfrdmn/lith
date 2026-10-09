// SPDX-License-Identifier: Apache-2.0

package fuse

// EvidenceRatioFor decides the #256 evidence-gate ratio in force for a read: the bound on a
// committed readahead window as a multiple of the bytes the handle has actually consumed.
//
// WHY THE GATE EXISTS. The window is sized by concurrency and by the NIC, never by how much
// of the object a handle is going to want. One process reading one variable of a
// multi-variable NetCDF-4 file was measured fetching the WHOLE object -- 54.03x over-fetch,
// equal to 1/coverage to within 0.4% on two objects (#284). The access is perfectly
// sequential WITHIN the variable, which is the point: sequential earns the full window, so
// the most sequential low-coverage reader pays most. Ratio 4 fixes it.
//
// WHY IT IS A FUNCTION OF THE REGION PAIR AND NOT OF LATENCY (#349). The gate's cost is
// RTT-scaled and falls on one shape -- a fast consumer reading most of an object at distance:
//
//	in-region, 6 concurrent slice readers : forced 358.6 MB in 3/3 (r = 1.00) against a
//	                                        default of 3281 / 1827 / 375 MB on three
//	                                        IDENTICAL cells, and no wall-clock cost
//	cross-region, 58.6 ms, dd whole-object: r = 1.96, zero overlap (min forced 15.80 s >
//	                                        max off 12.07 s, n = 4, p = 1/70)
//
// So the policy needs to know "near or far", and three rounds of measurement established that
// first-byte latency CANNOT tell it:
//
//   - The 8-sample median reads ~28 ms idle in-region and ~100 ms during the mount's own
//     prefetch burst -- above the old 50 ms bound AND above the 58.6 ms cross-region round
//     trip the bound's far side was anchored on. A busy near endpoint and an idle far one are
//     not separable on it.
//   - It is DOWNSTREAM OF THE DECISION, so the policy was bistable: gate off -> unbounded
//     window -> deeper burst -> higher latency -> gate stays off. The same workload came out
//     at 9.1x, 5.1x and 1.05x over-fetch on three IDENTICAL cells, and the gauge read on for
//     50-81% of ticks depending on the run. Amplification was not reproducible from
//     configuration, which is the one property a default has to have.
//   - A load-invariant floor (p10 over 256 fills) was built to escape that, and inherits it
//     one burst later: measured rising 18 -> 74 ms within 0.5 s of the gate turning off,
//     because the gate's own off-state removes the unqueued fills that make a floor a floor.
//     ANY statistic of our own fills has this shape.
//
// THE REGION PAIR HAS NONE OF IT. Fixed at mount, one IMDS lookup and no extra S3 call (the
// client resolved the bucket's region in order to sign at all), cannot be corrupted by load,
// and it makes the gate's state a function of CONFIGURATION rather than of timing. It also
// PREVENTS the regime rather than surviving it: in-region the gate is always on, so windows
// stay bounded and the burst never reaches the depth that corrupted the old signal.
//
// Rules, in order:
//
//  1. A POSITIVE --readahead-evidence-ratio wins, always. An operator who set it has said
//     what they want and is not second-guessed.
//  2. A NEGATIVE one forces the gate off. 0 means "decide for me", so there has to be a way
//     to say "off" and mean it.
//  3. REGION UNKNOWN -> off. IMDS is blocked on plenty of hardened images and a custom
//     --endpoint has no AWS region at all. Off is correct in every case measured: off-EC2 is
//     far, and R2 and other non-AWS endpoints cost 1.6-2.8x at every ratio tested. An on-prem
//     MinIO is near and loses the saving, which is what --readahead-evidence-ratio 4 is for.
//     Erring off costs only the saving; erring on is the measured ~2x at distance.
//  4. Same region -> ratio 4. Different region -> off.
//
// EXPORTED BECAUSE A SECOND GATE'S DEFAULT READS IT (#313). The tier-pressure gate
// (--prefetch-pressure-max) covers exactly the mounts this one does not, so cmd/lith derives
// its default from this function's RESULT rather than from the region pair directly. Two
// policies reading the same region booleans would be two policies that can drift; one reading
// the other's answer cannot. See pressureMaxFor in cmd/lith.
func EvidenceRatioFor(configured float64, nearRegion, regionKnown bool) float64 {
	if configured > 0 {
		return configured
	}
	if configured < 0 {
		return 0
	}
	if !regionKnown || !nearRegion {
		return 0
	}
	return defaultEvidenceRatio
}

// defaultEvidenceRatio is 4 because every cell that decided this used 4; no other value has
// been measured on any shape.
//
// THE LATENCY BOUND IS GONE, and its history is worth keeping because it cost three releases.
// v1.4.0 shipped it as 5 ms, derived from a 2.2 ms network ROUND TRIP, against a quantity that
// is a FIRST-BYTE latency -- in-region 28.2 ms median (p10 22.6, p90 42.7) -- so the default
// sat 4.5x below p10 and NO in-region mount ever engaged (#340). v1.5.0 corrected it to 50 ms,
// anchored on both measured sides. Then the quantity itself turned out to be load-sensitive
// and self-confirming (#349), so this is not a bound that was wrong by a factor: first-byte
// latency is the wrong INPUT, and the region pair replaced it.
//
// In one line: the unit error was caught by an external measurement, and the wrong-quantity
// error by a different external measurement two releases later. Neither was visible from the
// code, and both defaults looked reasonable when they shipped.
//
// What the gate buys where it engages, all measured on real S3: a single process reading one
// variable of a multi-variable NetCDF-4 file goes from fetching the WHOLE object to 2.65x of
// what it wanted, and six such readers concurrently go from 5-9x to 1.00x. What it costs is a
// fixed ~0.07-0.18 s on the one shape that pays, a fast consumer reading most of an object:
// r = 1.047 to 1.206 in-region across a 14.5x size range and two boxes.
const defaultEvidenceRatio = 4.0
