// SPDX-License-Identifier: Apache-2.0

// Package metrics defines lith's Prometheus metrics and an HTTP handler for
// the --metrics endpoint. See the pinned Design issue, §6. A nil *Metrics is
// safe to use: every method is a no-op, so components can run without metrics.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds lith's collectors. Construct with New; the zero/nil value is a
// safe no-op.
type Metrics struct {
	reg *prometheus.Registry

	cacheHits   *prometheus.CounterVec // tier=mem|disk
	cacheMiss   prometheus.Counter
	s3Bytes     prometheus.Counter
	s3Requests  *prometheus.CounterVec // op, status=ok|error
	inflight    prometheus.Gauge
	prefetchIss prometheus.Counter
	prefetchHit prometheus.Counter
	uncovered   prometheus.Counter
	straddle    prometheus.Counter
	staleTotal  prometheus.Counter
	fuseLatency *prometheus.HistogramVec // op
}

// New creates and registers the metric collectors on a fresh registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		cacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_cache_hits_total", Help: "Block cache hits by tier.",
		}, []string{"tier"}),
		cacheMiss: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_cache_misses_total", Help: "Block cache misses.",
		}),
		s3Bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_s3_bytes_total", Help: "Bytes fetched from S3.",
		}),
		s3Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_s3_requests_total", Help: "S3 requests by op and status.",
		}, []string{"op", "status"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lith_s3_inflight", Help: "In-flight S3 requests.",
		}),
		prefetchIss: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_issued_total", Help: "Blocks prefetched.",
		}),
		prefetchHit: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_used_total", Help: "Prefetched blocks later read on demand.",
		}),
		uncovered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_uncovered_total", Help: "Demand reads whose chunk was neither cached nor in flight.",
		}),
		straddle: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_read_straddle_total", Help: "Reads spanning a chunk boundary (assembled with a copy).",
		}),
		staleTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_stale_objects_total", Help: "Objects whose ETag no longer matched the index.",
		}),
		fuseLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "lith_fuse_op_seconds",
			Help:    "FUSE operation latency in seconds.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 4, 12), // ~1µs .. ~4s
		}, []string{"op"}),
	}
	reg.MustRegister(m.cacheHits, m.cacheMiss, m.s3Bytes, m.s3Requests,
		m.inflight, m.prefetchIss, m.prefetchHit, m.uncovered, m.straddle, m.staleTotal, m.fuseLatency)
	return m
}

// Handler returns the Prometheus HTTP handler for this registry.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// --- blockstore.Recorder implementation (all nil-safe) ---

func (m *Metrics) MemHit() {
	if m != nil {
		m.cacheHits.WithLabelValues("mem").Inc()
	}
}

func (m *Metrics) DiskHit() {
	if m != nil {
		m.cacheHits.WithLabelValues("disk").Inc()
	}
}

func (m *Metrics) Miss() {
	if m != nil {
		m.cacheMiss.Inc()
	}
}

// S3Get records one GET: its byte count and success/failure. The in-flight
// gauge is managed by Start/EndInflight around the actual call.
func (m *Metrics) S3Get(bytes int64, isErr bool) {
	if m == nil {
		return
	}
	status := "ok"
	if isErr {
		status = "error"
	}
	m.s3Requests.WithLabelValues("get", status).Inc()
	if bytes > 0 {
		m.s3Bytes.Add(float64(bytes))
	}
}

func (m *Metrics) StaleKey(string) {
	if m != nil {
		m.staleTotal.Inc()
	}
}

func (m *Metrics) PrefetchIssued() {
	if m != nil {
		m.prefetchIss.Inc()
	}
}

func (m *Metrics) PrefetchHit() {
	if m != nil {
		m.prefetchHit.Inc()
	}
}

func (m *Metrics) UncoveredMiss() {
	if m != nil {
		m.uncovered.Inc()
	}
}

// ReadStraddle records a read that spanned a chunk boundary (assembled copy).
func (m *Metrics) ReadStraddle() {
	if m != nil {
		m.straddle.Inc()
	}
}

// StartInflight marks the start of an in-flight S3 request (paired with
// EndInflight).
func (m *Metrics) StartInflight() {
	if m != nil {
		m.inflight.Inc()
	}
}

func (m *Metrics) EndInflight() {
	if m != nil {
		m.inflight.Dec()
	}
}

// ObserveFUSE records the latency of a FUSE op.
func (m *Metrics) ObserveFUSE(op string, seconds float64) {
	if m != nil {
		m.fuseLatency.WithLabelValues(op).Observe(seconds)
	}
}
