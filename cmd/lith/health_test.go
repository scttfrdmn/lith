// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scttfrdmn/lith/internal/metrics"
)

func TestHealthEndpoints(t *testing.T) {
	ready := newReadiness("starting: loading index")
	mux := newMetricsMux(metrics.New(), ready)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}

	// /healthz: 200 as soon as the server is up (liveness).
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	// /readyz before ready: 503 with the reason in the body.
	if code, body := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz (pre-ready) = %d, want 503", code)
	} else if !strings.Contains(body, "loading index") {
		t.Errorf("/readyz body = %q, want the not-ready reason", body)
	}
	// /metrics still served.
	if code, _ := get("/metrics"); code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", code)
	}

	// After serving begins: /readyz flips to 200.
	ready.set(true, "gateway serving")
	if code, body := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz (ready) = %d, want 200", code)
	} else if !strings.Contains(body, "ready") {
		t.Errorf("/readyz body = %q, want ready", body)
	}
}
