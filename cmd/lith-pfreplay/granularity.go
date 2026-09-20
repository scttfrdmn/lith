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
// pure counterfactual over the trace: for each candidate fetch unit G, charge each
// (key, G-extent) once mount-wide, count the fetches, and count the bytes no reader ever
// touches. Both axes of the trade, from the same rows, at no cost.
//
// Faithfulness note, and it is a real difference from globalScore's own cold loop: that
// loop indexes a read by r.off/chunkSize alone, so a read straddling a chunk boundary
// (~9% of HEMCO's) is charged one chunk when the mount fetches two. This sweep charges
// every extent the read covers, which is what the mount does. At G = chunkSize it
// therefore reports slightly MORE waste than globalScore's 1,257.3 MiB, and the gap is
// exactly the straddle undercount rather than a disagreement about the rule.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// granRow is one candidate fetch unit's outcome on one trace.
type granRow struct {
	unit         int64 // the fetch granularity in bytes
	fetches      int   // distinct (key, extent) fetches charged mount-wide
	hits         int   // eligible cold reads served free by an extent already fetched
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
			// Every extent the read covers, not just the one its offset lands in.
			first, last := r.off/g, (r.off+r.length-1)/g
			fetchedAny := false
			for ei := first; ei <= last; ei++ {
				if seen[r.key][ei] {
					continue
				}
				seen[r.key][ei] = true
				lo, hi := ei*g, ei*g+g
				if size := sizeOf[r.key]; size > 0 && hi > size {
					hi = size
				}
				if hi <= lo {
					continue
				}
				fetchedAny = true
				row.fetches++
				row.grossFetched += hi - lo
				row.netWaste += (hi - lo) - coveredBytes(allOf[r.key], lo, hi)
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
func reportGranularity(w io.Writer, label string, g []granRow) {
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
	fmt.Fprintf(w, "   -- cold-start fetch GRANULARITY sweep (%s): the trade the inversion makes\n", label)
	fmt.Fprintf(w, "      eligible first-run cold reads asked for %.1f MiB; today's %s chunk fetches %.1f MiB of it\n",
		mib(base.readBytes), human(chunkSize), mib(base.grossFetched))
	fmt.Fprintf(w, "      %-9s %9s %9s %11s %11s %9s   %s\n",
		"unit", "fetches", "free hits", "fetched MiB", "waste MiB", "waste %", "vs today")
	for _, r := range g {
		wpct := 0.0
		if r.grossFetched > 0 {
			wpct = 100 * float64(r.netWaste) / float64(r.grossFetched)
		}
		delta := ""
		if base.fetches > 0 && r.unit != chunkSize {
			saved := mib(base.netWaste - r.netWaste)
			extra := r.fetches - base.fetches
			per := 0.0
			if extra > 0 {
				per = (float64(base.netWaste-r.netWaste) / 1024) / float64(extra)
			}
			delta = fmt.Sprintf("saves %.1f MiB for %+d fetches (%.0f KiB/fetch)", saved, extra, per)
		} else if r.unit == chunkSize {
			delta = "today"
		}
		fmt.Fprintf(w, "      %-9s %9d %9d %11.1f %11.1f %8.1f%%   %s\n",
			human(r.unit), r.fetches, r.hits, mib(r.grossFetched), mib(r.netWaste), wpct, delta)
	}
	fmt.Fprintf(w, "      Read the KiB/fetch column as the exchange rate: bytes bought per extra\n")
	fmt.Fprintf(w, "      round-trip. It is not a verdict -- a request costs latency and money that\n")
	fmt.Fprintf(w, "      this trace cannot price, and the free-hits column is what shrinks.\n")
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

func human(b int64) string {
	switch {
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", b/(1<<20))
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", b/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
