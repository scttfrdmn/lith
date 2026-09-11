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

// TestReadSizeHistogramBuckets: the read-size histogram exposes the log2 buckets
// from 4 KiB to 8 MiB and counts each observation once (#65).
func TestReadSizeHistogramBuckets(t *testing.T) {
	m := New()
	m.ObserveReadSize(4096)    // -> le=4096
	m.ObserveReadSize(5000)    // -> le=8192
	m.ObserveReadSize(9 << 20) // -> +Inf (above 8 MiB)
	m.ObserveReadSize(-1)      // ignored
	if n, sum := m.ReadSizeStats(); n != 3 || sum != 4096+5000+(9<<20) {
		t.Fatalf("ReadSizeStats = %d/%d, want 3/%d", n, sum, 4096+5000+(9<<20))
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	// The 12 log2 buckets 4 KiB .. 8 MiB must all be present as `le=` boundaries.
	for _, le := range []string{"4096", "8192", "16384", "32768", "65536", "131072",
		"262144", "524288", "1.048576e+06", "2.097152e+06", "4.194304e+06", "8.388608e+06"} {
		if !strings.Contains(body, `lith_read_size_bytes_bucket{le="`+le+`"}`) {
			t.Errorf("read-size histogram missing bucket le=%s", le)
		}
	}
	if !strings.Contains(body, "lith_read_size_bytes_count 3") {
		t.Errorf("read-size histogram count != 3\n%s", body)
	}
}

// TestDistinctBytesReadFineAndCoarse: the per-object bitmap counts distinct
// touched extents at 64 KiB granularity (#118), coarsening to 64 MiB above 1 TiB,
// and dedups repeat reads of the same extent.
func TestDistinctBytesReadFineAndCoarse(t *testing.T) {
	const e = 64 << 10
	m := New()
	const tenMiB = 10 << 20
	m.MarkDistinctRead("a", 0, 4096, tenMiB)      // extent 0
	m.MarkDistinctRead("a", 1000, 2000, tenMiB)   // still extent 0 (dedup)
	m.MarkDistinctRead("a", e, 10, tenMiB)        // extent 1
	if got := m.DistinctBytesRead(); got != 2*e { // two distinct 64 KiB extents
		t.Fatalf("fine distinct = %d, want %d", got, 2*e)
	}
	// An object larger than 1 TiB uses 64 MiB granularity: a tiny read counts a
	// whole 64 MiB block, so the bitmap stays bounded on petabyte objects.
	const twoTiB = 2 << 40
	m.MarkDistinctRead("big", 0, 4096, twoTiB)
	if got := m.DistinctBytesRead(); got != 2*e+(64<<20) {
		t.Fatalf("coarse distinct = %d, want %d", got, 2*e+(64<<20))
	}
	// A read past EOF is clamped to the object size (no runaway bitmap).
	m.MarkDistinctRead("small", 0, 1<<30, 100) // size 100 -> one extent
	if got := m.DistinctBytesRead(); got != 2*e+(64<<20)+e {
		t.Fatalf("clamped distinct = %d, want %d", got, 2*e+(64<<20)+e)
	}
}

func TestNilMetricsReadHooks(t *testing.T) {
	var m *Metrics
	m.ObserveReadSize(4096)
	m.MarkDistinctRead("k", 0, 10, 100)
	if m.DistinctBytesRead() != 0 {
		t.Fatal("nil DistinctBytesRead should be 0")
	}
	if n, s := m.ReadSizeStats(); n != 0 || s != 0 {
		t.Fatal("nil ReadSizeStats should be 0/0")
	}
}
