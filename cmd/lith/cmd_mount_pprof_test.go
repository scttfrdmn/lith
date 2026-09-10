// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scttfrdmn/lith/internal/metrics"
)

// TestMetricsMuxHasNoPprof verifies F1: the --metrics mux serves /metrics but
// does NOT register the pprof surface. Uses httptest.ResponseRecorder against
// the mux's handler directly — no real network listener is opened.
func TestMetricsMuxHasNoPprof(t *testing.T) {
	mux := newMetricsMux(metrics.New())

	if got := statusFor(mux, "/metrics"); got != http.StatusOK {
		t.Errorf("/metrics on metrics mux = %d, want 200", got)
	}
	for _, p := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/trace",
		"/debug/pprof/symbol",
	} {
		if got := statusFor(mux, p); got != http.StatusNotFound {
			t.Errorf("%s on metrics mux = %d, want 404 (pprof must not be on the metrics mux)", p, got)
		}
	}
}

// TestPprofMuxHasPprof verifies the opt-in --pprof mux does register pprof and
// does NOT serve /metrics.
func TestPprofMuxHasPprof(t *testing.T) {
	mux := newPprofMux()

	// pprof.Index handles "/debug/pprof/"; a matched route is anything but 404.
	if got := statusFor(mux, "/debug/pprof/"); got == http.StatusNotFound {
		t.Errorf("/debug/pprof/ on pprof mux = 404, want it registered")
	}
	if got := statusFor(mux, "/metrics"); got != http.StatusNotFound {
		t.Errorf("/metrics on pprof mux = %d, want 404", got)
	}
}

func statusFor(mux *http.ServeMux, path string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}
