// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Scott Friedman

package main

// THE DISPATCH LEAD: how far ahead of the reader a prefetch was issued.
//
// #256's cross-region stall is not explained by the readahead window. The window series
// is bit-identical between a 20.3 s run and a 3.4 s run of the same arm, yet the evidence
// gate still triples the stall rate (wall > 9 s in 1 of 32 gate-off cells against 16 of
// 32 gate-on). So the gate acts through something the window does not parameterise.
//
// The candidate this measures: a demand read that arrives while its chunk's fetch is
// still in flight does not get a cache hit, it BLOCKS on that fetch (fetchExtents ->
// `<-cs.done`). It therefore waits on the PREFETCH's round trip rather than starting its
// own, and if the prefetch was issued only just before the read arrived, that wait is a
// full RTT for a fetch the reader could have started itself.
//
// Whether that happens depends on the LEAD: how long before the demand read its block was
// dispatched. A wide window dispatches far ahead, so the fetch has completed by the time
// the reader arrives — a hit. A narrow window dispatches just in time, so the reader
// catches the fetch in flight — a join. The evidence gate narrows the window early, which
// should shorten the lead.
//
// This is measurable offline, on traces already published, with no cluster and no timing:
// the lead in READS is a property of the decision sequence. It cannot prove the stall
// (that needs the join to actually cost a round trip, which is timing), but it tests the
// mechanism's NECESSARY CONDITION. If the gate does not shorten leads, the hypothesis is
// dead without spending anything.

import (
	"fmt"
	"sort"
)

// leadStats summarises the dispatch-lead distribution for one trace.
type leadStats struct {
	reads     int   // demand reads whose block was dispatched at some point
	neverDisp int   // demand reads on a block prefetch never dispatched
	selfFirst int   // reads that ARRIVED BEFORE the dispatch: they fetch it themselves
	leads     []int // per-read lead, in reads, for reads whose block was dispatched earlier
	blockSize int64
}

// scoreLead computes, for each demand read, how many reads earlier its block was
// dispatched. It reuses the dispatch reconstruction globalScore performs rather than
// repeating the detector replay — the reconstruction is the part that must not exist twice.
func scoreLead(ord []row, dispatches []dispatch, blockSize int64) leadStats {
	st := leadStats{blockSize: blockSize}

	// First dispatch position per (key, block). A block dispatched more than once is
	// suppressed by the store's singleflight after the first, so the first is what a
	// later read would join.
	type kb struct {
		key   string
		block int64
	}
	first := map[kb]int{}
	for _, d := range dispatches {
		k := kb{d.key, d.block}
		if at, ok := first[k]; !ok || d.at < at {
			first[k] = d.at
		}
	}

	for i, r := range ord {
		if r.path != "window" || r.length <= 0 || blockSize <= 0 {
			continue
		}
		at, ok := first[kb{r.key, r.off / blockSize}]
		if !ok {
			st.neverDisp++
			continue
		}
		if at >= i {
			// The reader reached this block before (or at) the dispatch that would have
			// covered it: it fetches the block itself and cannot join anything.
			st.selfFirst++
			continue
		}
		st.reads++
		st.leads = append(st.leads, i-at)
	}
	sort.Ints(st.leads)
	return st
}

func (s leadStats) pct(p float64) int {
	if len(s.leads) == 0 {
		return 0
	}
	i := int(p * float64(len(s.leads)-1))
	return s.leads[i]
}

// atMost counts reads whose lead is n or fewer — the ones most likely to find the fetch
// still in flight and block on it.
func (s leadStats) atMost(n int) int {
	return sort.SearchInts(s.leads, n+1)
}

func reportLead(label string, s leadStats) {
	total := s.reads + s.neverDisp + s.selfFirst
	if total == 0 {
		return
	}
	fmt.Printf("   -- dispatch LEAD (%s): how many reads before a demand read its block was prefetched\n", label)
	fmt.Printf("      %d window reads: %d dispatched earlier, %d reached before any dispatch, %d never dispatched\n",
		total, s.reads, s.selfFirst, s.neverDisp)
	if s.reads == 0 {
		return
	}
	fmt.Printf("      lead in reads   p1=%d  p10=%d  p50=%d  p90=%d  max=%d\n",
		s.pct(0.01), s.pct(0.10), s.pct(0.50), s.pct(0.90), s.leads[len(s.leads)-1])
	for _, n := range []int{1, 2, 4, 8, 16} {
		c := s.atMost(n)
		fmt.Printf("      lead <= %-2d reads : %6d (%.1f%% of dispatched-earlier reads)\n",
			n, c, 100*float64(c)/float64(s.reads))
	}
	fmt.Printf("      A SHORT lead is the risk: the fetch may still be in flight when the reader\n")
	fmt.Printf("      arrives, so the read blocks on the prefetch's round trip instead of its own.\n")
	fmt.Printf("      This is a count over the decision sequence, not a timing — it tests whether\n")
	fmt.Printf("      the mechanism is POSSIBLE, not whether it is what costs wall clock.\n")
}
