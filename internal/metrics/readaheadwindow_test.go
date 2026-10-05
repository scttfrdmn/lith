// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #298: the realized readahead window and its divisor were invisible at runtime —
// `--max-readahead` is logged as configured, and a reader holding 256 descriptors open
// reads at the window floor of 2 with nothing to show it. That cost a measured 6.38x the
// wall clock on bytes and requests that differ by 0.4% and 3%, so no byte or request
// counter could have caught it. These gauges are the thing that would have.
func TestReadaheadWindowGaugesExposeTheDivisor(t *testing.T) {
	m := New()
	window, handles, streams, evRatio, ttfb, measured := 223.0, 1.0, 1.0, 0.0, 0.0, 0.0
	floor := 0.0
	m.RegisterReadaheadWindow(
		func() float64 { return window },
		func() float64 { return handles },
		func() float64 { return streams },
		func() float64 { return evRatio },
		func() float64 { return ttfb },
		func() float64 { return measured },
		func() float64 { return floor },
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
		"lith_ttfb_median_seconds 0",
		// And the flag that disambiguates it: a median of 0 here means "nothing measured",
		// which is a different state from "measured, and fast". Both report the gate off.
		"lith_ttfb_measured 0",
		// The load-invariant floor (#349), 0 until enough fills have completed. Emitted
		// at zero so an absent series cannot be read as a missing feature.
		"lith_ttfb_floor_seconds 0",
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
	window, handles, streams, evRatio, ttfb, measured = 223, 1, 1, 0, 0.0282, 1
	got = scrape()
	for _, want := range []string{
		"lith_ttfb_median_seconds 0.0282",
		"lith_ttfb_measured 1",
		"lith_readahead_evidence_ratio 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("with a measured 28.2ms TTFB and the gate off, scrape missing %q", want)
		}
	}

	// THE #349 SEPARATION, as two scrapes the MEDIAN cannot tell apart. A busy in-region
	// mount and an idle cross-region one both report a median near 100 ms, so the evidence
	// policy reads them identically and turns itself off for both. Their FLOORS differ by
	// more than 2x, because queueing can only add: the near endpoint's fast fills are still
	// in the window, and the far one has none and cannot have any.
	ttfb, measured, floor = 0.1010, 1, 0.0226 // in-region, under its own prefetch burst
	nearLoaded := scrape()
	ttfb, measured, floor = 0.0986, 1, 0.0586 // cross-region, idle
	farIdle := scrape()
	if !strings.Contains(nearLoaded, "lith_ttfb_floor_seconds 0.0226") ||
		!strings.Contains(farIdle, "lith_ttfb_floor_seconds 0.0586") {
		t.Error("the floor gauge does not separate a loaded near endpoint from an idle far one")
	}
	if !strings.Contains(nearLoaded, "lith_ttfb_median_seconds 0.101") ||
		!strings.Contains(farIdle, "lith_ttfb_median_seconds 0.0986") {
		t.Fatal("fixture: both arms must report a median above the 50ms bound, or the " +
			"floor is not what distinguishes them")
	}

	// THE AMBIGUITY #341 NAMED, as two scrapes that differ in exactly one series. Both report
	// ratio 0; only the flag says which is which. The first is a mount that has not measured
	// its endpoint yet, where the gate is correctly inert. The second is #340: measured,
	// in-region, and still off because the bound was in the wrong unit.
	ttfb, measured = 0, 0
	unmeasured := scrape()
	ttfb, measured = 0.0282, 1
	measuredScrape := scrape()
	if !strings.Contains(unmeasured, "lith_ttfb_measured 0") ||
		!strings.Contains(measuredScrape, "lith_ttfb_measured 1") {
		t.Error("the measured flag does not distinguish a seed-valued median from a measurement")
	}
	if !strings.Contains(unmeasured, "lith_readahead_evidence_ratio 0") ||
		!strings.Contains(measuredScrape, "lith_readahead_evidence_ratio 0") {
		t.Fatal("fixture: both arms must report the gate off, or the flag is not what " +
			"distinguishes them")
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
	// 13.078 GB committed against an 8.256 GB tier is the reported collapse arm: 1.58x,
	// which is the information resident-unread cannot carry because it saturates at the
	// tier (#313).
	pressure := committed / 8.256e9
	m.RegisterPrefetchBudget(
		func() float64 { return committed },
		func() float64 { return limit },
		func() float64 { return unread },
		func() float64 { return pressure },
	)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	got := rec.Body.String()

	for _, want := range []string{
		"lith_prefetch_committed_bytes 1.3078e+10",
		"lith_prefetch_budget_bytes 4.128e+09",
		"lith_prefetch_unread_resident_bytes 8e+09",
		// Unbounded above by design: the gate's threshold is set against this, and a
		// value over 1 is the overcommit the tier cannot absorb.
		"lith_prefetch_pressure 1.58",
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

// #341: the raw first-byte latencies as a DISTRIBUTION, not only the rolling median the
// evidence policy reads.
//
// #340's bound was placed from the in-region spread -- p10 22.6 / median 28.2 / p90 42.7 ms --
// and that spread had to be recovered from a --timeline-csv, because the median was the only
// thing exported. A median alone also cannot show a bimodal endpoint, which is the shape that
// would make ANY single threshold wrong.
func TestTTFBHistogramExposesTheDistributionNotJustTheMedian(t *testing.T) {
	m := New()
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	// Registered before any observation, so an absent series cannot be read as a missing
	// feature -- the mistake this whole family of counters was added to stop.
	if got := scrape(); !strings.Contains(got, `lith_ttfb_seconds_count 0`) {
		t.Fatal("the histogram is not emitted before its first observation")
	}

	// The measured in-region distribution, as reported on #340.
	for _, ms := range []float64{22.6, 24.1, 26.0, 28.2, 29.9, 33.4, 42.7} {
		m.S3TTFB(time.Duration(ms * float64(time.Millisecond)))
	}
	got := scrape()
	for _, want := range []string{
		`lith_ttfb_seconds_count 7`,
		// THE BUCKET THAT DECIDES THE POLICY. The 50 ms bound is a bucket boundary on
		// purpose: le="0.05" vs _count is "is my endpoint inside the regime the default
		// assumes", answerable from one scrape and no quantile estimation.
		`lith_ttfb_seconds_bucket{le="0.05"} 7`,
		// And the resolution that matters either side of the median: Prometheus's default
		// buckets jump 0.025 -> 0.05, which would put the entire in-region distribution in
		// one bucket and show nothing.
		`lith_ttfb_seconds_bucket{le="0.025"} 2`,
		`lith_ttfb_seconds_bucket{le="0.03"} 5`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q", want)
		}
	}

	// A cross-region endpoint must land ABOVE the bound's bucket, or the one scrape that is
	// supposed to say "the default will not engage here" cannot say it.
	m.S3TTFB(58600 * time.Microsecond)
	got = scrape()
	if !strings.Contains(got, `lith_ttfb_seconds_bucket{le="0.05"} 7`) {
		t.Error("a 58.6ms cross-region sample fell inside the 50ms bucket")
	}
	if !strings.Contains(got, `lith_ttfb_seconds_bucket{le="0.06"} 8`) {
		t.Error("a 58.6ms sample is not resolved below 100ms; the cross-region regime is " +
			"indistinguishable from a pathological one")
	}

	// Nil-safe, like every other recorder here: a mount without --metrics must not panic.
	var nilM *Metrics
	nilM.S3TTFB(28 * time.Millisecond)
	// And a non-positive duration is not an observation -- recordTTFB drops those, so the
	// histogram must agree with the median it sits beside rather than skewing toward zero.
	m.S3TTFB(0)
	m.S3TTFB(-1)
	if got := scrape(); !strings.Contains(got, `lith_ttfb_seconds_count 8`) {
		t.Error("a zero or negative latency was observed as a sample")
	}
}

// #350: the wire histogram must be LABELLED by connection reuse, because that label is the
// hypothesis under test — a fresh mount's burst opens up to one connection per concurrent
// fill where a steady s3bench window reuses almost all of its.
func TestWireTTFBIsSplitByConnectionReuse(t *testing.T) {
	m := New()
	scrape := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}

	// A new connection paying dial + TLS, and a reused one that is not.
	m.S3WireTTFB(95*time.Millisecond, 62*time.Millisecond, false)
	m.S3WireTTFB(28*time.Millisecond, 20*time.Microsecond, true)

	got := scrape()
	for _, want := range []string{
		`lith_s3_wire_ttfb_seconds_count{conn="new"} 1`,
		`lith_s3_wire_ttfb_seconds_count{conn="reused"} 1`,
		`lith_s3_conn_acquire_seconds_count{conn="new"} 1`,
		`lith_s3_conn_acquire_seconds_count{conn="reused"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	// THE COMPARISON THE LABEL EXISTS FOR: the new-connection sample above the 50 ms bound
	// and the reused one below it. If both landed in one series there would be nothing to
	// compare, which is the state before this change.
	if !strings.Contains(got, `lith_s3_wire_ttfb_seconds_bucket{conn="new",le="0.05"} 0`) {
		t.Error("the 95ms new-connection sample is not above the 50ms bucket")
	}
	if !strings.Contains(got, `lith_s3_wire_ttfb_seconds_bucket{conn="reused",le="0.05"} 1`) {
		t.Error("the 28ms reused sample is not inside the 50ms bucket")
	}

	// A sub-microsecond acquisition must still be OBSERVED, not dropped. A pooled
	// connection legitimately acquires in well under a microsecond, and discarding those
	// would make the reused arm look slower than it is -- the exact comparison at issue.
	m.S3WireTTFB(27*time.Millisecond, 0, true)
	if !strings.Contains(scrape(), `lith_s3_conn_acquire_seconds_count{conn="reused"} 2`) {
		t.Error("a zero-duration acquisition was dropped; the reused arm would read slow")
	}

	var nilM *Metrics
	nilM.S3WireTTFB(time.Millisecond, time.Millisecond, true)
}
