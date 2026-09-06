// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
)

// Metrics must satisfy the blockstore Recorder interface.
var _ blockstore.Recorder = (*Metrics)(nil)

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	// None of these should panic on a nil receiver.
	m.MemHit()
	m.DiskHit()
	m.Miss()
	m.S3Get(10, false)
	m.StaleKey("k")
	m.PrefetchIssued()
	m.PrefetchHit()
	m.StartInflight()
	m.EndInflight()
	m.ObserveFUSE("read", 0.001)
}

func TestHandlerExposesCounters(t *testing.T) {
	m := New()
	m.MemHit()
	m.DiskHit()
	m.Miss()
	m.S3Get(4096, false)
	m.S3Get(0, true)
	m.PrefetchIssued()
	m.PrefetchHit()
	m.StaleKey("obj")
	m.ObserveFUSE("read", 0.002)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`lith_cache_hits_total{tier="mem"} 1`,
		`lith_cache_hits_total{tier="disk"} 1`,
		"lith_cache_misses_total 1",
		"lith_s3_bytes_total 4096",
		`lith_s3_requests_total{op="get",status="ok"} 1`,
		`lith_s3_requests_total{op="get",status="error"} 1`,
		"lith_prefetch_issued_total 1",
		"lith_prefetch_used_total 1",
		"lith_stale_objects_total 1",
		"lith_fuse_op_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
