// SPDX-License-Identifier: Apache-2.0

// Package metrics defines lith's Prometheus metrics and an HTTP handler for
// the --metrics endpoint. See the pinned Design issue, §6. A nil *Metrics is
// safe to use: every method is a no-op, so components can run without metrics.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds lith's collectors. Construct with New; the zero/nil value is a
// safe no-op.
type Metrics struct {
	reg *prometheus.Registry

	cacheHits     *prometheus.CounterVec // tier=mem|disk
	cacheMiss     prometheus.Counter
	s3Bytes       prometheus.Counter
	s3Requests    *prometheus.CounterVec // op, status=ok|error
	inflight      prometheus.Gauge
	prefetchIss   prometheus.Counter
	prefetchHit   prometheus.Counter
	uncovered     prometheus.Counter
	straddle      prometheus.Counter
	staleTotal    prometheus.Counter
	fuseLatency   *prometheus.HistogramVec // op
	prefetchWait  prometheus.Histogram
	pfHalved      prometheus.Counter
	pfResetRand   prometheus.Counter
	pfEvictUnread prometheus.Counter
	sibPrefetch   prometheus.Counter
	sibUnread     prometheus.Counter
	formatDetect  *prometheus.CounterVec
	formatPlane   prometheus.Counter
	formatReplan  prometheus.Counter
	formatIdxPfB  prometheus.Counter
	formatRanges  *prometheus.CounterVec // format
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
		prefetchWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "lith_prefetch_sem_wait_seconds",
			Help:    "Time a prefetch fill spent blocked acquiring the prefetch semaphore.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 4, 12), // ~1µs .. ~4s
		}),
		pfHalved: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_window_halved_total", Help: "Readahead-window halvings from an out-of-band seek.",
		}),
		pfResetRand: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_reset_random_total", Help: "Prefetch detector collapses to random (two seeks, no progress between).",
		}),
		pfEvictUnread: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_evicted_unread_total", Help: "Prefetched chunks evicted before a demand read consumed them (thrash; #55).",
		}),
		sibPrefetch: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_sibling_prefetch_total", Help: "Sibling objects prefetched by directory-walk readahead (#63).",
		}),
		sibUnread: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_sibling_prefetch_unread_total", Help: "Sibling-prefetched objects that fell out of the pending window without being opened (#63 accuracy guardrail).",
		}),
		formatDetect: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_format_detect_total", Help: "Format-aware access plans detected, by format (#70).",
		}, []string{"format"}),
		formatPlane: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_format_plane_chunks_total", Help: "Chunks prefetched by the Zarr grid-plane selection plan (#70 tier 2).",
		}),
		formatReplan: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_format_replan_total", Help: "Times an out-of-plane open triggered a new grid-plane selection (#70 tier 2).",
		}),
		formatIdxPfB: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_format_index_prefetch_bytes_total", Help: "Bytes of external index prefetched whole on index-file open (#107 tier 1).",
		}),
		formatRanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_format_plan_ranges_total", Help: "Data byte ranges prefetched by an index-resolved plan, by format (#107 tier 2).",
		}, []string{"format"}),
	}
	reg.MustRegister(m.cacheHits, m.cacheMiss, m.s3Bytes, m.s3Requests,
		m.inflight, m.prefetchIss, m.prefetchHit, m.uncovered, m.straddle, m.staleTotal, m.fuseLatency, m.prefetchWait,
		m.pfHalved, m.pfResetRand, m.pfEvictUnread, m.sibPrefetch, m.sibUnread, m.formatDetect,
		m.formatPlane, m.formatReplan, m.formatIdxPfB, m.formatRanges)
	return m
}

// FormatIndexPrefetchBytes records bytes of external index prefetched whole on
// index-file open (#107 tier 1). Nil-safe.
func (m *Metrics) FormatIndexPrefetchBytes(n int64) {
	if m != nil && n > 0 {
		m.formatIdxPfB.Add(float64(n))
	}
}

// FormatPlanRanges records one data byte range prefetched by an index-resolved
// plan (#107 tier 2). Nil-safe.
func (m *Metrics) FormatPlanRanges(format string) {
	if m != nil {
		m.formatRanges.WithLabelValues(format).Inc()
	}
}

// FormatPlaneChunks records n chunks dispatched by the grid-plane plan (#70
// tier 2). Nil-safe.
func (m *Metrics) FormatPlaneChunks(n int64) {
	if m != nil && n > 0 {
		m.formatPlane.Add(float64(n))
	}
}

// FormatReplan records that an out-of-plane open triggered a new plane (#70
// tier 2). Nil-safe.
func (m *Metrics) FormatReplan() {
	if m != nil {
		m.formatReplan.Inc()
	}
}

// FormatDetect records that a format-aware plan was detected for an object
// (#70). Nil-safe.
func (m *Metrics) FormatDetect(format string) {
	if m != nil {
		m.formatDetect.WithLabelValues(format).Inc()
	}
}

// SiblingPrefetch records n sibling objects dispatched by directory-walk
// readahead (#63). Nil-safe.
func (m *Metrics) SiblingPrefetch(n int64) {
	if m != nil && n > 0 {
		m.sibPrefetch.Add(float64(n))
	}
}

// SiblingPrefetchUnread records n sibling-prefetched objects that fell out of
// the pending window without being opened (the #63 accuracy guardrail).
// Nil-safe.
func (m *Metrics) SiblingPrefetchUnread(n int64) {
	if m != nil && n > 0 {
		m.sibUnread.Add(float64(n))
	}
}

// RegisterQueueDepth registers a gauge that samples f on each scrape (used for
// the disk write-behind queue depth).
func (m *Metrics) RegisterQueueDepth(f func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_disk_write_queue_depth", Help: "Chunks queued for the write-behind disk writer.",
	}, f))
}

// Handler returns the Prometheus HTTP handler for this registry.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// PrefetchWait records the time a prefetch fill blocked on the prefetch
// semaphore (optional blockstore.Recorder extension).
func (m *Metrics) PrefetchWait(d time.Duration) {
	if m != nil {
		m.prefetchWait.Observe(d.Seconds())
	}
}

// PrefetchSeeks adds a handle's window-halving and random-reset counts (called
// once per handle at Release). Nil-safe.
func (m *Metrics) PrefetchSeeks(halvings, resets int64) {
	if m == nil {
		return
	}
	if halvings > 0 {
		m.pfHalved.Add(float64(halvings))
	}
	if resets > 0 {
		m.pfResetRand.Add(float64(resets))
	}
}

// PrefetchEvictedUnread records a prefetched chunk evicted before it was read
// (optional blockstore.Recorder extension; the #55 thrash signal).
func (m *Metrics) PrefetchEvictedUnread() {
	if m != nil {
		m.pfEvictUnread.Inc()
	}
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
