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
	if got[0].fetches != 2 {
		t.Errorf("straddling read: fetches = %d, want 2 (chunk 0 and chunk 1)", got[0].fetches)
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
		if cur.fetches < prev.fetches {
			t.Errorf("unit %d made %d fetches < larger unit %d's %d: fetches must not fall as the unit shrinks",
				cur.unit, cur.fetches, prev.unit, prev.fetches)
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
	if got[0].fetches != 1 {
		t.Errorf("fetches = %d, want 1", got[0].fetches)
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
