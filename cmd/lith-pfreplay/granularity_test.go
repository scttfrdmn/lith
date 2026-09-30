// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

import "testing"

// coldRead builds a row the cold-tax rule accepts: first-run, cold, window path.
func coldRead(fh uint64, key string, size, off, length int64, seq int64) row {
	return row{seq: seq, fh: fh, key: key, size: size, off: off, length: length,
		path: "window", before: "cold", after: "cold"}
}

// The straddle case is the whole reason this sweep exists as its own pass rather than a
// parameter on globalScore's loop: that loop indexes a read by off/chunkSize and charges
// ONE extent, so a read crossing a boundary is undercounted. This sweep must charge both.
// Asserted directly, because "it charges every extent the read covers" is exactly the kind
// of claim that reads as true and measures as false.
func TestSweepChargesEveryExtentAReadCovers(t *testing.T) {
	const mib = 1 << 20
	// One 128 KiB read starting 64 KiB before a 1 MiB boundary: it covers chunk 0 and 1.
	rows := []row{coldRead(1, "k", 4*mib, mib-64*1024, 128*1024, 1)}
	got := sweepGranularity(rows, chunkSize, []int64{mib})
	if len(got) != 1 {
		t.Fatalf("want 1 unit, got %d", len(got))
	}
	if got[0].gets != 2 {
		t.Errorf("straddling read: GETs = %d, want 2 (one fill per chunk touched)", got[0].gets)
	}
	if want := int64(2 * mib); got[0].grossFetched != want {
		t.Errorf("grossFetched = %d, want %d", got[0].grossFetched, want)
	}
	// Only the read's own 128 KiB is ever touched, so everything else is waste.
	if want := int64(2*mib - 128*1024); got[0].netWaste != want {
		t.Errorf("netWaste = %d, want %d", got[0].netWaste, want)
	}
}

// The sweep's two axes must move in opposite directions monotonically, or the table cannot
// be read as a trade. A unit that both fetched less and wasted less than a larger unit
// would mean the accounting is not charging the smaller unit for its extra requests.
func TestSweepTradesMonotonically(t *testing.T) {
	const mib = 1 << 20
	var rows []row
	// Ten handles each reading a sparse 64 KiB slice out of every 1 MiB of a 16 MiB
	// object: the HEMCO shape, low coverage with heavy chunk sharing.
	var seq int64
	for fh := uint64(1); fh <= 10; fh++ {
		for c := int64(0); c < 16; c++ {
			seq++
			rows = append(rows, coldRead(fh, "k", 16*mib, c*mib+int64(fh)*64*1024, 64*1024, seq))
		}
	}
	units := []int64{mib, 512 * 1024, 256 * 1024, 64 * 1024}
	got := sweepGranularity(rows, chunkSize, units)
	if len(got) != len(units) {
		t.Fatalf("want %d rows, got %d", len(units), len(got))
	}
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.netWaste > prev.netWaste {
			t.Errorf("unit %d wasted %d > larger unit %d's %d: waste must not grow as the unit shrinks",
				cur.unit, cur.netWaste, prev.unit, prev.netWaste)
		}
		if cur.gets < prev.gets {
			t.Errorf("unit %d made %d GETs < larger unit %d's %d: requests must not fall as the unit shrinks",
				cur.unit, cur.gets, prev.unit, prev.gets)
		}
		if cur.hits > prev.hits {
			t.Errorf("unit %d had %d free hits > larger unit %d's %d: free hits must not grow as the unit shrinks",
				cur.unit, cur.hits, prev.unit, prev.hits)
		}
	}
	// Every unit scores the same population, or the rows are not comparable.
	for i := range got {
		if got[i].readBytes != got[0].readBytes {
			t.Errorf("unit %d scored %d read bytes, unit %d scored %d: the population must not vary by unit",
				got[i].unit, got[i].readBytes, got[0].unit, got[0].readBytes)
		}
	}
}

// The sweep must not widen the population the cold-tax rule selects. A footer read, a
// cold re-entry, or a read at or above a chunk is excluded by globalScore and must stay
// excluded here, or the sweep's baseline would not be what the mount does today.
func TestSweepInheritsTheColdTaxPopulation(t *testing.T) {
	const mib = 1 << 20
	rows := []row{
		coldRead(1, "k", 8*mib, 0, 64*1024, 1), // eligible
		{seq: 2, fh: 2, key: "k", size: 8 * mib, off: mib, length: 64 * 1024, path: "footer", before: "cold"},   // not the window path
		{seq: 3, fh: 3, key: "k", size: 8 * mib, off: 2 * mib, length: 2 * mib, path: "window", before: "cold"}, // >= a chunk
		{seq: 4, fh: 4, key: "k", size: 8 * mib, off: 3 * mib, length: 64 * 1024, path: "window", before: "sequential"},
		// fh 4 has been classified, so its later cold read is a re-entry, not first-run.
		{seq: 5, fh: 4, key: "k", size: 8 * mib, off: 4 * mib, length: 64 * 1024, path: "window", before: "cold"},
	}
	got := sweepGranularity(rows, chunkSize, []int64{mib})
	if got[0].readBytes != 64*1024 {
		t.Errorf("readBytes = %d, want %d: only the one eligible read may be scored",
			got[0].readBytes, 64*1024)
	}
	if got[0].gets != 1 {
		t.Errorf("GETs = %d, want 1", got[0].gets)
	}
}

func TestParseUnitsSortsDescendingAndDropsJunk(t *testing.T) {
	got := parseUnits("256KiB, 1MiB,,64KiB, banana, 0, 2048B")
	want := []int64{1 << 20, 256 * 1024, 64 * 1024, 2048}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (descending, junk dropped not coerced)", got, want)
		}
	}
	if u := parseUnits(""); len(u) != 0 {
		t.Errorf("empty spec must yield no units, got %v", u)
	}
}

// THE CORRECTION THIS SWEEP WAS REWRITTEN FOR.
//
// The first cut counted one "fetch" per granularity unit and published that as the request
// cost. lith does not work that way: fetchExtents runs once per CHUNK and fillExtentSpan
// issues EXACTLY ONE ranged GET spanning first-missing to last-missing extent. So a read
// covering many units is one request, not one per unit — and the published table overstated
// the cost of going byte-exact.
//
// Asserted on a single read wide enough that the two counts cannot coincide.
func TestSweepCountsRequestsNotUnits(t *testing.T) {
	const mib = 1 << 20
	// One 512 KiB read, scored at a 64 KiB unit: 8 units, but one contiguous GET.
	rows := []row{coldRead(1, "k", 8*mib, 0, 512*1024, 1)}
	got := sweepGranularity(rows, chunkSize, []int64{64 * 1024})
	if got[0].gets != 1 {
		t.Errorf("GETs = %d, want 1: one ranged GET covers the whole contiguous span", got[0].gets)
	}
	if got[0].units != 8 {
		t.Errorf("units = %d, want 8 (512 KiB / 64 KiB)", got[0].units)
	}
	if got[0].grossFetched != 512*1024 {
		t.Errorf("grossFetched = %d, want %d", got[0].grossFetched, 512*1024)
	}
}

// fillExtentSpan fetches ONE span from the first missing extent to the last, so a resident
// extent sitting between two missing ones is RE-FETCHED and re-marked filled. The sweep has
// to charge that, or it under-reports bytes on exactly the interleaved case the shared
// cache creates.
func TestSweepChargesTheSpanIncludingResidentUnitsInside(t *testing.T) {
	const mib = 1 << 20
	// Read A takes the middle 64 KiB. Read B then wants the surrounding 192 KiB, whose
	// missing extents straddle A's: one GET must span all three units, re-fetching A's.
	rows := []row{
		coldRead(1, "k", 8*mib, 64*1024, 64*1024, 1),
		coldRead(2, "k", 8*mib, 0, 192*1024, 2),
	}
	got := sweepGranularity(rows, chunkSize, []int64{64 * 1024})
	if got[0].gets != 2 {
		t.Fatalf("GETs = %d, want 2 (one per read)", got[0].gets)
	}
	// A fetched 64 KiB; B's span covers all 192 KiB including A's resident middle.
	if want := int64(64*1024 + 192*1024); got[0].grossFetched != want {
		t.Errorf("grossFetched = %d, want %d: the span must include the resident unit inside it",
			got[0].grossFetched, want)
	}
	// Only 3 distinct units ever became resident, even though 4 unit-fetches were paid for.
	if got[0].units != 3 {
		t.Errorf("units = %d, want 3: units counts residency, not bytes paid", got[0].units)
	}
}
