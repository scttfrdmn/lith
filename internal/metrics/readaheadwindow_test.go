// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// #298: the realized readahead window and its divisor were invisible at runtime —
// `--max-readahead` is logged as configured, and a reader holding 256 descriptors open
// reads at the window floor of 2 with nothing to show it. That cost a measured 6.38x the
// wall clock on bytes and requests that differ by 0.4% and 3%, so no byte or request
// counter could have caught it. These gauges are the thing that would have.
func TestReadaheadWindowGaugesExposeTheDivisor(t *testing.T) {
	m := New()
	window, handles, streams := 223.0, 1.0, 1.0
	m.RegisterReadaheadWindow(
		func() float64 { return window },
		func() float64 { return handles },
		func() float64 { return streams },
	)

	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	got := scrape()
	for _, want := range []string{
		"lith_readahead_window_blocks 223",
		"lith_open_handles 1",
		"lith_streaming_handles 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q", want)
		}
	}

	// The case that was invisible: a crowded mount at the window floor. The gauges must
	// move together, because either alone is ambiguous — a window of 2 could be a tight
	// budget or a crowded mount, and the divisor is what distinguishes them.
	window, handles, streams = 2, 256, 246
	got = scrape()
	for _, want := range []string{
		"lith_readahead_window_blocks 2",
		"lith_open_handles 256",
		"lith_streaming_handles 246",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("after the divisor collapsed the window, scrape missing %q", want)
		}
	}
	if strings.Contains(got, "lith_readahead_window_blocks 223") {
		t.Error("the window gauge is latched at its configured value; it must report the realized one")
	}

	// THE DIAGNOSTIC #301 turns on: descriptors far above streams. Both counts must be
	// scrapeable independently, because that GAP is what the old divisor charged for and
	// what distinguishes "too many readers for the budget" (raise it) from "idle
	// descriptors are stealing depth" (which, since #301, no longer happens).
	window, handles, streams = 223, 288, 1
	got = scrape()
	for _, want := range []string{
		"lith_open_handles 288",
		"lith_streaming_handles 1",
		"lith_readahead_window_blocks 223",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("with 288 descriptors against 1 stream, scrape missing %q", want)
		}
	}
}

// #313: the three prefetch-budget gauges, and specifically that the resident-unread one is
// SEPARATELY observable from committed.
//
// An external measurement found committed/tier separating clean from collapsed runs at 1.20
// vs 1.21 across two boxes 12x apart in RAM, and could not explain why committed plateaued
// near 1.55x tier. Both resolve if committed = resident-unread + in-flight: only the resident
// half can evict anything, and it cannot exceed the tier by construction. These must therefore
// be scrapeable as two numbers, not one — a deployment needs to subtract them.
func TestPrefetchBudgetGaugesSeparateResidentFromInFlight(t *testing.T) {
	m := New()
	committed, limit, unread := 13.078e9, 4.128e9, 8.0e9
	m.RegisterPrefetchBudget(
		func() float64 { return committed },
		func() float64 { return limit },
		func() float64 { return unread },
	)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	got := rec.Body.String()

	for _, want := range []string{
		"lith_prefetch_committed_bytes 1.3078e+10",
		"lith_prefetch_budget_bytes 4.128e+09",
		"lith_prefetch_unread_resident_bytes 8e+09",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q", want)
		}
	}

	// The gauges must not be the same series under two names: the whole point is that
	// committed exceeds resident-unread by the bytes in flight, and that difference is what
	// a deployment reads to tell "the tier is full of unread prefetch" from "a lot is in
	// flight and the tier is fine".
	if committed <= unread {
		t.Fatal("fixture: committed must exceed resident-unread for this to test anything")
	}
	if strings.Contains(got, "lith_prefetch_unread_resident_bytes 1.3078e+10") {
		t.Error("the resident-unread gauge is reporting the committed total")
	}
}

// #316: the coverage gate's rejections must be scrapeable.
//
// Concurrent readers of ONE object have each other's reads absorbed by the shared kernel
// page cache, so each handle sees a punctate offset stream, the #221 coverage gate forces it
// Random, and nothing prefetches. Measured at 243x slower with byte amplification of 1.001 —
// so bytes, requests, and `prefetch_issued_total` all look correct or better. This counter
// and `lith_prefetch_evicted_unread_total` are the only things that move.
func TestLowCoverageCounterIsScrapeable(t *testing.T) {
	m := New()
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	// Registered at zero, so an absent series cannot be read as a missing feature.
	if got := scrape(); !strings.Contains(got, "lith_prefetch_low_coverage_total 0") {
		t.Error("the counter is not emitted at zero")
	}

	m.PrefetchLowCoverage(430)
	m.PrefetchLowCoverage(0) // a handle that never tripped the gate must not change it
	if got := scrape(); !strings.Contains(got, "lith_prefetch_low_coverage_total 430") {
		t.Error("scrape missing lith_prefetch_low_coverage_total 430")
	}

	// Nil-safe, like every other recorder here: a mount without --metrics must not panic.
	var nilM *Metrics
	nilM.PrefetchLowCoverage(1)
}
