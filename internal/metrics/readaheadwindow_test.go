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
	window, handles, streams, evRatio, ttfb := 223.0, 1.0, 1.0, 0.0, 0.0
	m.RegisterReadaheadWindow(
		func() float64 { return window },
		func() float64 { return handles },
		func() float64 { return streams },
		func() float64 { return evRatio },
		func() float64 { return ttfb },
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
		// Off by default, and emitted so an absent line cannot be read as a missing feature.
		"lith_readahead_evidence_ratio 0",
		// The policy's INPUT, 0 until a fill has measured the endpoint (#341). Without this
		// series, #340's never-engaging default could only be diagnosed from the source.
		"lith_s3_ttfb_seconds 0",
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

	// THE PAIRING THAT WOULD HAVE CAUGHT #340: a mount reporting a real in-region first-byte
	// latency with the evidence ratio still 0. Before #341 only the ratio was visible, so
	// "the gate is off" and "the gate is off because its bound is in the wrong unit" looked
	// identical on a scrape.
	window, handles, streams, evRatio, ttfb = 223, 1, 1, 0, 0.0282
	got = scrape()
	for _, want := range []string{
		"lith_s3_ttfb_seconds 0.0282",
		"lith_readahead_evidence_ratio 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("with a measured 28.2ms TTFB and the gate off, scrape missing %q", want)
		}
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

// #316: the coverage gate's two rejection counters, and that they are LIVE.
//
// Concurrent readers of ONE object have each other's reads absorbed by the shared kernel page
// cache, so each handle advances in order yet looks punctate, the #221 coverage gate refuses
// it a window, and nothing prefetches. Measured at 243x slower with byte amplification of
// 1.001 — so bytes, requests and prefetch_issued_total all look correct or better.
//
// The two counters must be separable: a SEEK landing forced Random is the gate working as
// designed, while contiguous progress denied a window is the #316 shape. One number cannot
// tell those apart, and the first version of this shipped with only the first.
func TestCoverageCountersAreSeparableAndLive(t *testing.T) {
	m := New()
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	// Both registered at zero, so an absent series cannot be read as a missing feature.
	got := scrape()
	for _, want := range []string{
		"lith_prefetch_low_coverage_total 0",
		"lith_prefetch_coverage_held_total 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("not emitted at zero: %q", want)
		}
	}

	// THE #316 SHAPE: contiguous reads held, no seek rejections at all. The two must move
	// independently — an external deployment needs exactly this pairing to tell "my readers
	// share a file" from "my readers are scattered walks".
	h := m.PrefetchHandleFor(">64MiB")
	for i := 0; i < 430; i++ {
		h.Record(PrefetchDelta{CoverageHeld: 1})
	}
	got = scrape()
	if !strings.Contains(got, "lith_prefetch_coverage_held_total 430") {
		t.Error("the held counter did not accumulate per read")
	}
	if !strings.Contains(got, "lith_prefetch_low_coverage_total 0") {
		t.Error("a contiguous rejection wrongly incremented the SEEK counter — the two are " +
			"not separable, and separating them is the whole point")
	}

	// A scattered walk moves the other one.
	h.Record(PrefetchDelta{Seek: 7})
	if got := scrape(); !strings.Contains(got, "lith_prefetch_low_coverage_total 7") {
		t.Error("the seek counter did not accumulate")
	}

	// Nil-safe at both levels, like every other recorder here: a mount without --metrics
	// must not panic, and the resolved handle it hands out must be safe to Record.
	var nilM *Metrics
	nilM.PrefetchHandleFor(">64MiB").Record(PrefetchDelta{Seek: 1, CoverageHeld: 1})
	var nilH *PrefetchHandle
	nilH.Record(PrefetchDelta{Seek: 1})
}
