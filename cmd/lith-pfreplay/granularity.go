// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

// THE COLD-START GRANULARITY SWEEP.
//
// The key-level fit closed the last fix direction that attached a hint to an object: a
// predictor of the cold tax exists (coverage, rho -0.69) and still does not license an
// allow-list, because what it predicts is a property of ~70-80% of the working set. The
// direction left standing is to make the granularity commitment conditional -- "unknown
// until proven sequential" at internal/fuse/fs.go:778-786, where a cold handle today
// fetches a whole 1 MiB chunk for a 100 KiB read and only a handle already proven Random
// is served byte-exact.
//
// But the shared-cache accounting also says that inverting the default outright is not
// obviously right, and says so with a number nobody had looked at: of HEMCO's 24,801
// first-run cold small reads, 20,610 (83%) are SUPPRESSED -- they land in a chunk another
// handle already paid for, and cost nothing. The chunk is the right unit 83% of the time
// by hit count and the wrong unit by byte count (only 63% of the cold chunk bytes fetched
// are ever read by anyone). Byte-exact serving would save the 1,257 MiB and turn every one
// of those 20,610 free hits into its own round-trip -- the #124 GET explosion, one level
// down.
//
// So the question is not WHETHER to commit a chunk, it is AT WHAT GRANULARITY. That is a
// pure counterfactual over the trace, on both axes of the trade, from the same rows.
//
// THE REQUEST AXIS, and the correction that produced this version. The first cut of this
// sweep counted one "fetch" per granularity UNIT and reported that as the cost, which
// overstates requests -- at 64 KiB it claimed 42,570 where the real number is far lower.
// lith does not issue a GET per extent. fetchExtents is called once per CHUNK and
// fillExtentSpan then issues EXACTLY ONE ranged GET, spanning first-missing to
// last-missing extent (blockstore/fill.go, extent.go:missingByteSpan) -- so it re-fetches
// any resident extent that happens to lie between two missing ones, and marks it filled.
// The consequence for the trade is large: going byte-exact does not multiply requests by
// the number of extents, it multiplies them by how often a LATER read has to come back to
// a chunk an earlier one no longer fully covers. So `gets` is the cost axis and `units`
// is the bytes axis, and they are separate fields because conflating them was the bug.
//
// Faithfulness note, and a real difference from globalScore's own cold loop: that loop
// indexes a read by r.off/chunkSize alone, so a read straddling a chunk boundary (~9% of
// HEMCO's) is charged one chunk where the mount fills two. This sweep fills per chunk
// touched, which is what the mount does. At G = chunkSize it therefore reports slightly
// MORE waste than globalScore's 1,257.3 MiB, and the gap is the straddle undercount
// rather than a disagreement about the rule.

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// granRow is one candidate fetch unit's outcome on one trace.
type granRow struct {
	unit int64 // the fetch granularity in bytes
	// gets is the number of S3 REQUESTS: fillExtentSpan issues exactly one ranged GET per
	// per-chunk fill, spanning first-missing to last-missing unit. This is the cost axis,
	// and counting units here instead (as the first version of this sweep did) overstates
	// requests by the number of units a single GET happens to cover.
	gets int
	// units is how many granularity units became resident -- the bytes axis, not a request
	// count. Kept distinct from gets precisely because conflating them was the bug.
	units        int
	hits         int   // eligible cold reads that needed no GET at all: every wanted unit resident
	grossFetched int64 // bytes fetched by those fetches (EOF-clamped)
	netWaste     int64 // of those, bytes no reader on the key ever reads
	readBytes    int64 // bytes the eligible cold reads actually asked for
}

// sweepGranularity re-charges the cold-start tax at each candidate fetch unit.
//
// The eligibility rule is globalScore's, unchanged and deliberately not re-derived: a
// first-run read in the cold state on the window path, smaller than a chunk and within the
// byte-exact threshold. Only the FETCH UNIT varies. Everything the rule excludes -- cold
// re-entries, footer and parts reads, reads at or above a chunk -- stays excluded, so the
// sweep is a counterfactual about granularity and nothing else.
func sweepGranularity(rows []row, byteExact int64, units []int64) []granRow {
	// Decision order, and the per-key read set that defines "never read by anyone".
	ord := make([]row, len(rows))
	copy(ord, rows)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].seq < ord[j].seq })

	allOf := map[string][]row{}
	sizeOf := map[string]int64{}
	for _, r := range ord {
		allOf[r.key] = append(allOf[r.key], r)
		if r.size > sizeOf[r.key] {
			sizeOf[r.key] = r.size
		}
	}

	// Which reads the rule makes eligible, computed once so every unit scores the same
	// population. A unit that changed the population would be measuring two things.
	notFirstRun := map[uint64]bool{}
	eligible := make([]row, 0, len(ord))
	for _, r := range ord {
		if r.before != "cold" {
			notFirstRun[r.fh] = true
			continue
		}
		if r.path != "window" || r.length > byteExact || r.length >= chunkSize {
			continue
		}
		if notFirstRun[r.fh] {
			continue
		}
		eligible = append(eligible, r)
	}

	out := make([]granRow, 0, len(units))
	for _, g := range units {
		if g <= 0 {
			continue
		}
		row := granRow{unit: g}
		seen := map[string]map[int64]bool{}
		for _, r := range eligible {
			row.readBytes += r.length
			if seen[r.key] == nil {
				seen[r.key] = map[int64]bool{}
			}
			size := sizeOf[r.key]
			// One fill per CHUNK the read touches, because the extent bitmap and the
			// singleflight claim are both per chunk: fetchExtents is called per chunk and
			// each call issues at most one GET.
			fetchedAny := false
			for ci := r.off / chunkSize; ci <= (r.off+r.length-1)/chunkSize; ci++ {
				cLo, cHi := ci*chunkSize, ci*chunkSize+chunkSize
				if size > 0 && cHi > size {
					cHi = size
				}
				if cHi <= cLo {
					continue
				}
				// What this fill WANTS in this chunk. At g >= chunkSize that is the whole
				// chunk — today's `sequential` commitment at fs.go:778-786. Below it, only
				// the g-aligned units the read actually overlaps.
				wLo, wHi := cLo, cHi
				if g < chunkSize {
					rLo, rHi := max64(r.off, cLo), min64(r.off+r.length, cHi)
					wLo, wHi = (rLo/g)*g, ((rHi+g-1)/g)*g
					wLo, wHi = max64(wLo, cLo), min64(wHi, cHi)
				}
				// The missing units, and the span one GET would cover. fillExtentSpan
				// fetches ONE contiguous range from the first missing unit to the last,
				// re-fetching any resident unit in between and marking it filled -- so the
				// span, not the missing set, is what gets paid for.
				fLo, fHi := int64(-1), int64(-1)
				for u := wLo; u < wHi; u += g {
					if !seen[r.key][u/g] {
						if fLo < 0 {
							fLo = u
						}
						fHi = min64(u+g, cHi)
					}
				}
				if fLo < 0 {
					continue // every wanted unit already resident: no GET
				}
				fetchedAny = true
				row.gets++
				row.grossFetched += fHi - fLo
				row.netWaste += (fHi - fLo) - coveredBytes(allOf[r.key], fLo, fHi)
				for u := fLo; u < fHi; u += g {
					if !seen[r.key][u/g] {
						row.units++
					}
					seen[r.key][u/g] = true
				}
			}
			if !fetchedAny {
				row.hits++
			}
		}
		out = append(out, row)
	}
	return out
}

// reportGranularity prints the sweep as the trade it is: bytes saved against round-trips
// added, both relative to what the mount does today at G = chunkSize.
func reportGranularity(label string, g []granRow) {
	if len(g) == 0 {
		return
	}
	// The baseline is today's behaviour: the largest unit, which is the chunk.
	var base granRow
	for _, r := range g {
		if r.unit == chunkSize {
			base = r
		}
	}
	fmt.Printf("   -- cold-start fetch GRANULARITY sweep (%s): the trade the inversion makes\n", label)
	fmt.Printf("      eligible first-run cold reads asked for %.1f MiB; today's %s chunk fetches %.1f MiB of it\n",
		mib(base.readBytes), human(chunkSize), mib(base.grossFetched))
	fmt.Printf("      %-9s %8s %8s %9s %11s %11s %8s   %s\n",
		"unit", "GETs", "units", "free hits", "fetched MiB", "waste MiB", "waste %", "vs today")
	for _, r := range g {
		wpct := 0.0
		if r.grossFetched > 0 {
			wpct = 100 * float64(r.netWaste) / float64(r.grossFetched)
		}
		delta := ""
		if base.gets > 0 && r.unit != chunkSize {
			saved := mib(base.netWaste - r.netWaste)
			extra := r.gets - base.gets
			per := 0.0
			if extra > 0 {
				per = (float64(base.netWaste-r.netWaste) / 1024) / float64(extra)
			}
			delta = fmt.Sprintf("saves %.1f MiB for %+d GETs (%.0f KiB/GET)", saved, extra, per)
		} else if r.unit == chunkSize {
			delta = "today"
		}
		fmt.Printf("      %-9s %8d %8d %9d %11.1f %11.1f %7.1f%%   %s\n",
			human(r.unit), r.gets, r.units, r.hits, mib(r.grossFetched), mib(r.netWaste), wpct, delta)
	}
	fmt.Printf("      GETs is REQUESTS, one per per-chunk fill (fillExtentSpan issues exactly one\n")
	fmt.Printf("      ranged GET, spanning first-missing to last-missing unit). `units` is the bytes\n")
	fmt.Printf("      axis and is NOT a request count -- conflating the two overstated requests.\n")
	fmt.Printf("      Read KiB/GET as the exchange rate: bytes bought per extra round-trip. Not a\n")
	fmt.Printf("      verdict -- a request costs latency this trace cannot price, and free hits shrink.\n")
}

// parseUnits reads the -granularity list. Sizes come back in DESCENDING order so the
// chunk-sized baseline is the first row and every later row reads as a departure from
// today. An unparseable entry is dropped rather than silently coerced to a size that
// would then be reported as a measurement.
func parseUnits(s string) []int64 {
	var out []int64
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		mult := int64(1)
		switch {
		case strings.HasSuffix(f, "MiB"):
			mult, f = 1<<20, strings.TrimSuffix(f, "MiB")
		case strings.HasSuffix(f, "KiB"):
			mult, f = 1<<10, strings.TrimSuffix(f, "KiB")
		case strings.HasSuffix(f, "B"):
			f = strings.TrimSuffix(f, "B")
		}
		n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "-granularity: ignoring unparseable size %q\n", f)
			continue
		}
		out = append(out, n*mult)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

func mib(b int64) float64 { return float64(b) / (1 << 20) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// human formats a byte count. Exact binary multiples print exactly (a 1 MiB fetch unit
// must read as "1MiB", not "1.0MiB"); anything else — a decimal capacity like 24GB, or a
// per-shard share that divides unevenly — gets one decimal in the largest binary unit that
// fits, rather than a raw byte count nobody can read at a glance.
func human(b int64) string {
	for _, u := range []struct {
		n int64
		s string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		// Exact only when the quotient is also a sane magnitude: 24 GB is an exact
		// multiple of 1 KiB, and "23437500KiB" is not a unit anyone reads.
		if q := b / u.n; b >= u.n && b%u.n == 0 && q < 1024 {
			return fmt.Sprintf("%d%s", q, u.s)
		}
	}
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
