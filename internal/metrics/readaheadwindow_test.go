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
