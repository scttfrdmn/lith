// SPDX-License-Identifier: Apache-2.0

// Package metrics defines lith's Prometheus metrics and an HTTP handler for
// the --metrics endpoint. See the pinned Design issue, §6. A nil *Metrics is
// safe to use: every method is a no-op, so components can run without metrics.
package metrics

import (
	"net/http"
	"sync"
	"sync/atomic"
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
	pfLowCoverage prometheus.Counter
	pfCovHeld     prometheus.Counter
	pfPressHeld   prometheus.Counter
	ttfb          prometheus.Histogram
	wireTTFB      prometheus.Histogram
	straddle      prometheus.Counter
	staleTotal    prometheus.Counter
	fuseLatency   *prometheus.HistogramVec // op
	prefetchWait  prometheus.Histogram
	pfHalved      prometheus.Counter
	pfEvClamped   *prometheus.CounterVec // evidence-gate clamps by object size class (#256)
	pfEvWithheld  prometheus.Counter     // blocks withheld by those clamps (#256)
	pfDeEstab     *prometheus.CounterVec // establishments lost, by object size class (#256)
	pfResetRand   prometheus.Counter
	pfEvictUnread prometheus.Counter
	sibPrefetch   prometheus.Counter
	sibUnread     prometheus.Counter
	formatDetect  *prometheus.CounterVec
	formatPlane   prometheus.Counter
	formatReplan  prometheus.Counter
	formatIdxPfB  prometheus.Counter
	formatRanges  *prometheus.CounterVec // format

	fillPartial prometheus.Counter     // partial (sub-chunk) fills (#118)
	fillBytes   *prometheus.CounterVec // fill bytes by kind=plan|demand|whole|gap (#118/#124)
	fillRuns    prometheus.Counter     // coalesced fill-batch range GETs (#124)
	fillGap     prometheus.Counter     // bytes fetched only to close coalesce gaps (#124)
	fillBatchSz prometheus.Histogram   // ranges per fill batch (#124/session 30)
	fillInfl    prometheus.Gauge       // fill-batch runs currently in flight (#31)
	fillInflPk  prometheus.Gauge       // high-water mark of fill-batch runs in flight (#31)
	nfsClients  prometheus.Gauge       // active NFS gateway clients (#143)
	nfsSeqState prometheus.Gauge       // per-path sequential-read states held (#197)
	nfsOps      *prometheus.CounterVec // NFS ops by op= (#143)
	nfsReadByte prometheus.Counter     // bytes served over NFS READ (#143)
	backFrames  prometheus.Counter     // CargoShip frames fetched (#94)
	backReuse   prometheus.Counter     // CargoShip frames served from the decoded-frame cache (#137)
	backDecomp  prometheus.Counter     // CargoShip bytes decompressed (#94)
	backCkFail  prometheus.Counter     // CargoShip per-frame checksum failures (#94)
	readSize    prometheus.Histogram   // FUSE read request sizes (#65)
	readN       atomic.Int64           // read count, for bench mean
	readSum     atomic.Int64           // summed read bytes, for bench mean
	distinct    *distinctReads         // per-object chunk bitmaps (#65)
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
			Name: "lith_prefetch_issued_total", Help: "Chunks prefetched (1 MiB cache chunks, not blocks -- one record per chunk dispatched).",
		}),
		prefetchHit: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_used_total", Help: "Prefetched chunks TOUCHED by a later demand read (same 1 MiB unit as lith_prefetch_issued_total). A CHUNK-TOUCH count, not a byte count: a 64 KiB read marks the whole 1 MiB chunk used, so used/issued OVERSTATES byte follow-through, and overstates it most for the scattered readers where prefetch is least useful. Measured case: 89% by this ratio against at most ~25% of the prefetched bytes actually read. Do not read it as a byte efficiency (#256).",
		}),
		pfLowCoverage: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_low_coverage_total",
			Help: "Reads the #221 coverage gate forced Random on a SEEK landing: a scattered walk, which is the gate working as designed. Incremented live, per read. Pair with lith_prefetch_coverage_held_total -- that one rising while this stays flat is the #316 shape (#221, #316).",
		}),
		pfPressHeld: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_pressure_held_total",
			Help: "Prefetch dispatches DROPPED by the pressure gate (#313). Zero means either the gate is off or it never had to fire, and those are different states -- read lith_prefetch_pressure to tell them apart. Dropping a dispatch costs nothing a reader waits on: demand reads do not go through the prefetch path, so under pressure lith stops guessing and keeps serving.",
		}),
		wireTTFB: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "lith_s3_wire_ttfb_seconds",
			Help: "First-byte latency AS THE TRANSPORT SEES IT, from httptrace.GotFirstResponseByte -- one sample per HTTP attempt, retries included (#350). Compare against lith_ttfb_seconds, which is measured in the fill path: if this is low while that is high, the delay is ABOVE the wire (SDK middleware, response deserialization, or the fill goroutine waiting to be rescheduled) and not at the endpoint. An external differential probe put a mount and lith-s3bench on the same box, bucket, endpoint and part size at the same moment and measured 30 ms against >50 ms, with the box at 20-34% CPU, so the gap is inside lith -- this is the seam that says where. Only populated when --wire-ttfb is set, because it requires the plain-HTTP-client path.",
			Buckets: []float64{
				0.001, 0.005, 0.010, 0.020, 0.025, 0.030, 0.040, 0.050,
				0.060, 0.080, 0.100, 0.200, 0.500, 1.0,
			},
		}),
		ttfb: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "lith_ttfb_seconds",
			Help: "S3 first-byte latency per fill. The DISTRIBUTION, not the rolling median the evidence policy reads -- #340's latency bound was placed from in-region p90 (42.7 ms) against a median of 28.2 ms, and that spread had to be recovered from a --timeline-csv because nothing exported it (#341). Buckets span in-region (~28 ms) through cross-region (>58 ms) first-byte latencies.",
			// Centred on the measured regimes rather than Prometheus's defaults, which top
			// out at 10 s and have nothing between 25 ms and 50 ms -- the interval the
			// evidence bound sits in.
			Buckets: []float64{
				0.001, 0.005, 0.010, 0.020, 0.025, 0.030, 0.040, 0.050,
				0.060, 0.080, 0.100, 0.200, 0.500, 1.0,
			},
		}),
		pfCovHeld: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_coverage_held_total",
			Help: "Reads that made CONTIGUOUS progress and were denied a readahead window anyway, because coverage had not confirmed the access tiles. This rising while lith_prefetch_low_coverage_total stays flat is the signature of #316: concurrent readers of ONE object have their siblings' reads absorbed by the shared kernel page cache, so each handle advances in order yet looks punctate and never establishes -- measured at 243x slower with byte amplification of 1.001. Nothing counted this before: deEstablish() only counts an establishment that existed, and these handles are refused while still provisional.",
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
		pfEvClamped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_prefetch_evidence_clamped_total",
			Help: "Times the #256 evidence gate held a readahead window below the configured max, by object size class. Zero unless --readahead-evidence-ratio is set.",
		}, []string{"size_class"}),
		pfEvWithheld: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_prefetch_evidence_withheld_blocks_total",
			Help: "Readahead blocks the #256 evidence gate withheld (window the detector wanted minus the window consumption earned).",
		}),
		pfDeEstab: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_prefetch_deestablished_total",
			Help: "Times a handle lost an establishment it had, by object size class (#256). NOT the same as lith_prefetch_reset_random_total, which counts collapses to the Random state and is 20-30x larger; a high value here tracks a reader whose prefetch works (frequent productive re-anchoring), not a wasteful one.",
		}, []string{"size_class"}),
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
		fillPartial: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_fill_partial_total", Help: "Sub-chunk (sparse) fills — fewer than all extents of a 1 MiB chunk (#118).",
		}),
		fillBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_fill_bytes_total", Help: "Bytes fetched from S3 by fill kind: plan (format projection plan), demand-batch (coalesced union of a demand-read burst), demand (single unfilled-extent read), whole (streaming/prefetch), gap (fetched only to close a sub-coalesce-gap hole) (#118/#124/#125).",
		}, []string{"kind"}),
		fillRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_fill_runs_total", Help: "Coalesced fill-batch range GETs (#124).",
		}),
		fillGap: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_fill_gap_bytes_total", Help: "Bytes fetched only to close sub-coalesce-gap holes between plan ranges (#124).",
		}),
		fillBatchSz: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "lith_fill_batch_size",
			Help:    "Number of ranges coalesced per fill batch (#124/session 30).",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1 .. 2048
		}),
		fillInfl: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lith_fill_inflight", Help: "Fill-batch range GETs currently in flight (#31).",
		}),
		fillInflPk: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lith_fill_inflight_peak", Help: "High-water mark of fill-batch range GETs in flight since mount (#31).",
		}),
		nfsClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lith_nfs_clients", Help: "Active NFS gateway clients (#143).",
		}),
		nfsSeqState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "lith_nfs_seq_states", Help: "Per-path sequential-read states currently held by the gateway (#197).",
		}),
		nfsOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lith_nfs_ops_total", Help: "NFS gateway operations by type (#143).",
		}, []string{"op"}),
		nfsReadByte: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_nfs_read_bytes_total", Help: "Bytes served over NFS READ (#143).",
		}),
		backFrames: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_backing_frames_fetched_total", Help: "CargoShip zstd frames fetched from packed chunks (#94).",
		}),
		backReuse: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_backing_frame_reuse_total", Help: "CargoShip frame fills served from the decoded-frame cache — no GET, no decode (#137).",
		}),
		backDecomp: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_backing_decompress_bytes_total", Help: "Bytes decompressed from CargoShip frames (#94).",
		}),
		backCkFail: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lith_backing_checksum_fail_total", Help: "CargoShip per-frame content-checksum failures (served as EIO/stale, never silently) (#94).",
		}),
		readSize: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "lith_read_size_bytes",
			Help:    "FUSE read request sizes in bytes (#65).",
			Buckets: prometheus.ExponentialBuckets(4096, 2, 12), // 4 KiB .. 8 MiB
		}),
		distinct: newDistinctReads(),
	}
	reg.MustRegister(m.cacheHits, m.cacheMiss, m.s3Bytes, m.s3Requests,
		m.inflight, m.prefetchIss, m.prefetchHit, m.uncovered, m.straddle, m.staleTotal, m.fuseLatency, m.prefetchWait,
		m.pfHalved, m.pfResetRand, m.pfLowCoverage, m.pfCovHeld, m.pfPressHeld, m.ttfb, m.wireTTFB, m.pfEvClamped, m.pfEvWithheld, m.pfDeEstab, m.pfEvictUnread, m.sibPrefetch, m.sibUnread, m.formatDetect,
		m.formatPlane, m.formatReplan, m.formatIdxPfB, m.formatRanges, m.readSize,
		m.fillPartial, m.fillBytes, m.fillRuns, m.fillGap, m.fillBatchSz, m.fillInfl, m.fillInflPk,
		m.backFrames, m.backReuse, m.backDecomp, m.backCkFail,
		m.nfsClients, m.nfsSeqState, m.nfsOps, m.nfsReadByte)
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_distinct_bytes_read",
		Help: "Distinct object bytes read through the mount, from per-object touched-extent bitmaps. " +
			"Granularity is 64 KiB (one sparse-fill extent, #118); objects larger than 1 TiB use a coarser 64 MiB granularity to bound the bitmap. " +
			"Tracking is capped at 2^20 distinct objects (beyond that new objects are not counted).",
	}, func() float64 { return float64(m.distinct.totalBytes()) }))
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

// RegisterReadaheadWindow registers gauges for the readahead depth a handle will actually
// be given and the two handle counts that bear on it (#298, #301).
//
// perHandleWindow is clamp(prefetchBudgetBlocks / streamingHandles, 2, max-readahead).
// streamingHandles counts handles the detector is actually prefetching for; openHandles
// counts every open file DESCRIPTOR on the mount, across processes, including descriptors
// never read.
//
// BOTH are exported, and their gap is the point. openHandles USED to be the divisor, which
// meant a reader holding files open collapsed its own and everyone else's prefetch depth:
// 256 open descriptors drove the window to its floor of 2 and cost 6.38x the wall clock on
// +0.4% bytes and +3% requests — invisible to every byte and request counter, which is why
// it took an external workload and a dozen gates to find, and why both GCHP production
// mounts ran there. #301 changed the divisor's input, so the gap is now diagnostic rather
// than causal: wide means many idle descriptors (no longer a problem), narrow-and-crowded
// means genuinely more streams than the budget can fund (raise --prefetch-budget).
func (m *Metrics) RegisterReadaheadWindow(window, openHandles, streamingHandles, evidenceRatio, ttfbSeconds, ttfbMeasured, ttfbFloor func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_readahead_window_blocks",
		Help: "Readahead depth in blocks a handle is currently given: clamp(prefetch-budget/streaming-handles, 2, --max-readahead). This is the EFFECTIVE window; --max-readahead is only an upper bound on it (#298).",
	}, window))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_open_handles",
		Help: "Open file descriptors on the mount, counted per DESCRIPTOR across all processes including descriptors never read. This USED to be the readahead divisor, which is why holding files open shrank prefetch depth (#298); it no longer is (#301). Compare with lith_streaming_handles.",
	}, openHandles))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_streaming_handles",
		Help: "Handles being prefetched for: established sequential streams. This is the divisor for the readahead window (#301). Its gap from lith_open_handles is what the old divisor over-charged for -- on the workload that found this, ~288 descriptors against ~48 streams.",
	}, streamingHandles))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_ttfb_median_seconds",
		Help: "Rolling median S3 FIRST-BYTE latency -- the exact input evidenceRatioFor reads. Not a network round trip: in-region this is ~28 ms against an RTT of ~2 ms, and confusing the two is what made v1.4.0's evidence default never engage (#340). Pair with lith_ttfb_measured: this reads 0 when nothing has been measured yet (#341).",
	}, ttfbSeconds))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_ttfb_floor_seconds",
		Help: "A LOAD-INVARIANT estimate of the endpoint's first-byte latency: the 10th percentile over a 256-fill window. 0 until enough fills have completed. Queueing can only ADD to a first-byte latency, so the low end of a window is a lower bound on what the endpoint costs and load cannot raise it -- unlike lith_ttfb_median_seconds, which reads ~28 ms on an idle in-region mount and ~100 ms during that same mount's prefetch burst (#349). NOTHING READS THIS YET: it is here so the deciding measurement can be taken before the evidence policy is moved onto it.",
	}, ttfbFloor))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_ttfb_measured",
		Help: "1 once a fill has measured the endpoint's first-byte latency, 0 before. Without it, a mount in the evidence gate's \"off\" regime looks identical from outside whether nothing has been measured yet, the measurement is above the bound, or the policy is wrong -- and #340 was the second of those, diagnosable only from the source (#341).",
	}, ttfbMeasured))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_readahead_evidence_ratio",
		Help: "The #256 evidence-gate ratio in force: a committed readahead window may not exceed this multiple of the bytes a handle has actually consumed. 0 means the gate is off, which is the default and is also what a mount reports before any fill has measured the endpoint's latency. Non-zero without --readahead-evidence-ratio set means the latency-derived policy engaged (#284).",
	}, evidenceRatio))
}

// RegisterPrefetchBudget registers gauges for what --prefetch-budget actually bounds:
// bytes prefetch has committed and nothing has consumed, against the budget's own limit
// (#301).
//
// Until these existed the budget was enforced only by a PROXY -- the per-handle window
// times the open-handle count -- and the proxy was the only observable. That matters
// because the proxy is what made the divisor charge for idle file descriptors, at a
// measured 6.38x wall-clock cost, and nobody could see whether the real quantity was
// anywhere near its limit. The divisor now counts streams (#301), but these remain the
// only view of the quantity itself.
//
// Do not check these against the proxy. An estimator built from dispatch counts returns
// the standing window by construction, so "resident ~= window x handles" is an identity
// that will pass whether or not either number is right.
func (m *Metrics) RegisterPrefetchBudget(resident, limit, unreadResident, pressure func() float64) {
	if m == nil {
		return
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_prefetch_committed_bytes",
		Help: "Bytes prefetch has committed and nothing has consumed, which is exactly resident-unread PLUS queued-for-a-slot PLUS on-the-wire. Charged at DISPATCH, before the prefetch and S3 semaphores, so --inflight-bytes does NOT bound it: that bounds the wire, not the queue. This is not a resident-memory figure. At rest it equals lith_prefetch_unread_resident_bytes exactly; use that one, not this, to judge memory-tier pressure (#313, #320).",
	}, resident))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_prefetch_budget_bytes",
		Help: "The --prefetch-budget limit in bytes (default 50%% of --mem-cache). lith_prefetch_committed_bytes is what is actually held against it.",
	}, limit))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_prefetch_unread_resident_bytes",
		Help: "Bytes HELD IN THE MEMORY TIER that nothing has read. This -- not lith_prefetch_committed_bytes -- is the quantity eviction-before-read is about: committed counts from dispatch and so includes bytes still in flight, which cannot evict anything. Compare against --mem-cache, not --prefetch-budget: the collapse condition is this approaching tier CAPACITY, at which point every arriving chunk must evict an unread one (#313).",
	}, unreadResident))
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "lith_prefetch_pressure",
		Help: "Outstanding prefetch commitment as a fraction of the MEMORY TIER's realized capacity -- the quantity --prefetch-pressure-max admits against (#313). Unbounded above by design: 8.0 means eight tiers' worth has been promised and most of it must evict something unread on arrival. Read it with lith_prefetch_unread_resident_bytes, which saturates at the tier and so cannot tell a mild overcommit from a 12x one, and with lith_prefetch_pressure_held_total, which says whether the gate is actually doing anything.",
	}, pressure))
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

// PrefetchPressureHeld records one dispatch dropped by the pressure gate (#313).
// Nil-safe; blockstore's optional pressureRecorder extension.
func (m *Metrics) PrefetchPressureHeld() {
	if m != nil {
		m.pfPressHeld.Inc()
	}
}

// S3WireTTFB observes one HTTP attempt's first-byte latency as measured by the transport
// (#350). Nil-safe.
func (m *Metrics) S3WireTTFB(d time.Duration) {
	if m != nil && d > 0 {
		m.wireTTFB.Observe(d.Seconds())
	}
}

// S3TTFB observes one fill's first-byte latency (blockstore's optional ttfbRecorder
// extension). Non-positive durations are not observations, matching recordTTFB's own guard,
// so the histogram agrees with the median gauge beside it. Nil-safe.
func (m *Metrics) S3TTFB(d time.Duration) {
	if m != nil && d > 0 {
		m.ttfb.Observe(d.Seconds())
	}
}

// PrefetchDelta is one read's contribution to the per-handle prefetch counters.
//
// Recorded PER READ. The four counters below were all folded in once per handle at
// rawFS.Release, which made every one of them useless on a running job: an external 48-rank
// deployment holding its handles open for the whole run saw zeroes in every 2 Hz sample and
// non-zero values only in the final scrape, after the job was killed. #319 fixed that for the
// coverage counters; this does the rest.
type PrefetchDelta struct {
	Seek             int64 // coverage gate forced Random on a seek landing (#221)
	CoverageHeld     int64 // contiguous progress denied a window (#316)
	Halvings         int64 // window halvings (#40)
	Resets           int64 // collapses to Random
	EvidenceHeld     int64 // the #256 evidence gate held the window below max
	EvidenceWithheld int64 // blocks withheld across those holds
	DeEstablished    int64 // establishment lost, counted only where there was one
}

// Since returns the field-wise difference d - b, for a caller holding cumulative snapshots
// taken either side of one decision.
func (d PrefetchDelta) Since(b PrefetchDelta) PrefetchDelta {
	return PrefetchDelta{
		Seek:             d.Seek - b.Seek,
		CoverageHeld:     d.CoverageHeld - b.CoverageHeld,
		Halvings:         d.Halvings - b.Halvings,
		Resets:           d.Resets - b.Resets,
		EvidenceHeld:     d.EvidenceHeld - b.EvidenceHeld,
		EvidenceWithheld: d.EvidenceWithheld - b.EvidenceWithheld,
		DeEstablished:    d.DeEstablished - b.DeEstablished,
	}
}

// Empty reports whether this delta would change nothing, so a caller can skip the record on
// the overwhelming majority of reads.
func (d PrefetchDelta) Empty() bool {
	return d == PrefetchDelta{}
}

// PrefetchHandle is the resolved set of labelled counters for one handle's size class. It
// exists so a per-read record is a handful of atomic adds rather than a WithLabelValues map
// lookup under a registry mutex on a hot path.
//
// Resolving it at Open also makes the labelled series EXIST from the first open rather than
// the first close. A labelled counter that only appears when it fires is indistinguishable in
// a scrape from "the binary lacks the feature", "a different label value", or "the mount was
// never opened" -- the present-and-zero-versus-absent trap of #253, reported from the field
// for exactly these metrics.
type PrefetchHandle struct {
	m         *Metrics
	evClamped prometheus.Counter
	deEstab   prometheus.Counter
}

// PrefetchHandleFor resolves the counters for a size class and touches them at zero. Nil-safe,
// and the returned value is nil-safe to Record.
func (m *Metrics) PrefetchHandleFor(sizeClass string) *PrefetchHandle {
	if m == nil {
		return nil
	}
	return &PrefetchHandle{
		m:         m,
		evClamped: m.pfEvClamped.WithLabelValues(sizeClass),
		deEstab:   m.pfDeEstab.WithLabelValues(sizeClass),
	}
}

// Record adds one read's deltas. Nil-safe.
func (h *PrefetchHandle) Record(d PrefetchDelta) {
	if h == nil || h.m == nil {
		return
	}
	if d.Seek > 0 {
		h.m.pfLowCoverage.Add(float64(d.Seek))
	}
	if d.CoverageHeld > 0 {
		h.m.pfCovHeld.Add(float64(d.CoverageHeld))
	}
	if d.Halvings > 0 {
		h.m.pfHalved.Add(float64(d.Halvings))
	}
	if d.Resets > 0 {
		h.m.pfResetRand.Add(float64(d.Resets))
	}
	if d.EvidenceHeld > 0 {
		h.evClamped.Add(float64(d.EvidenceHeld))
	}
	if d.EvidenceWithheld > 0 {
		h.m.pfEvWithheld.Add(float64(d.EvidenceWithheld))
	}
	if d.DeEstablished > 0 {
		h.deEstab.Add(float64(d.DeEstablished))
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

// FillPartial records a sub-chunk (sparse) fill (#118). Nil-safe.
func (m *Metrics) FillPartial() {
	if m != nil {
		m.fillPartial.Inc()
	}
}

// FillBytes records bytes fetched by a fill of the given kind (#118). Nil-safe.
func (m *Metrics) FillBytes(kind string, n int64) {
	if m != nil && n > 0 {
		m.fillBytes.WithLabelValues(kind).Add(float64(n))
	}
}

// FillRun records one coalesced fill-batch range GET (#124). Nil-safe.
func (m *Metrics) FillRun() {
	if m != nil {
		m.fillRuns.Inc()
	}
}

// FillGapBytes records bytes fetched only to close a coalesce gap (#124). Nil-safe.
func (m *Metrics) FillGapBytes(n int64) {
	if m != nil && n > 0 {
		m.fillGap.Add(float64(n))
	}
}

// FillBatchSize records the number of ranges in a fill batch (#124). Nil-safe.
func (m *Metrics) FillBatchSize(n int) {
	if m != nil && n > 0 {
		m.fillBatchSz.Observe(float64(n))
	}
}

// FillInflight adjusts the in-flight fill-run gauge (#31). Nil-safe.
func (m *Metrics) FillInflight(delta float64) {
	if m != nil {
		m.fillInfl.Add(delta)
	}
}

// FillInflightPeak raises the in-flight fill-run high-water gauge to n (#31).
// The peak only grows over a mount, so Set with a new maximum is monotonic.
// Nil-safe.
func (m *Metrics) FillInflightPeak(n float64) {
	if m != nil {
		m.fillInflPk.Set(n)
	}
}

// BackingFramesFetched records n CargoShip frames fetched (#94). Nil-safe.
func (m *Metrics) BackingFramesFetched(n int64) {
	if m != nil && n > 0 {
		m.backFrames.Add(float64(n))
	}
}

// NFSClients adds delta to the active-NFS-clients gauge (#143). Nil-safe.
func (m *Metrics) NFSClients(delta int) {
	if m != nil {
		m.nfsClients.Add(float64(delta))
	}
}

// NFSSeqStates sets the gauge of per-path sequential-read states the gateway
// currently holds (#197). Nil-safe.
func (m *Metrics) NFSSeqStates(n int) {
	if m != nil {
		m.nfsSeqState.Set(float64(n))
	}
}

// NFSOp increments the NFS op counter for op (#143). Nil-safe.
func (m *Metrics) NFSOp(op string) {
	if m != nil {
		m.nfsOps.WithLabelValues(op).Inc()
	}
}

// NFSReadBytes records n bytes served over NFS READ (#143). Nil-safe.
func (m *Metrics) NFSReadBytes(n int64) {
	if m != nil && n > 0 {
		m.nfsReadByte.Add(float64(n))
	}
}

// BackingFrameReuse records n CargoShip frame fills served from the decoded-frame
// cache (no GET, no decode) (#137). Nil-safe.
func (m *Metrics) BackingFrameReuse(n int64) {
	if m != nil && n > 0 {
		m.backReuse.Add(float64(n))
	}
}

// BackingDecompressBytes records n bytes decompressed from CargoShip frames
// (#94). Nil-safe.
func (m *Metrics) BackingDecompressBytes(n int64) {
	if m != nil && n > 0 {
		m.backDecomp.Add(float64(n))
	}
}

// BackingChecksumFail records a CargoShip per-frame checksum failure (#94).
// Nil-safe.
func (m *Metrics) BackingChecksumFail() {
	if m != nil {
		m.backCkFail.Inc()
	}
}

// ObserveReadSize records one FUSE read request size (#65). Nil-safe.
func (m *Metrics) ObserveReadSize(n int64) {
	if m == nil || n < 0 {
		return
	}
	m.readSize.Observe(float64(n))
	m.readN.Add(1)
	m.readSum.Add(n)
}

// MarkDistinctRead marks the object chunks covered by a read of [off,off+length)
// on an object of the given size, for the distinct-bytes-read metric (#65).
// Nil-safe.
func (m *Metrics) MarkDistinctRead(key string, off, length, size int64) {
	if m != nil {
		m.distinct.mark(key, off, length, size)
	}
}

// DistinctBytesRead returns the distinct object bytes read so far (#65). Nil-safe.
func (m *Metrics) DistinctBytesRead() int64 {
	if m == nil {
		return 0
	}
	return m.distinct.totalBytes()
}

// ReadSizeStats returns the read count and summed read bytes (for a mean; #65).
// Nil-safe.
func (m *Metrics) ReadSizeStats() (count, sumBytes int64) {
	if m == nil {
		return 0, 0
	}
	return m.readN.Load(), m.readSum.Load()
}

// --- distinct-bytes-read tracking (#65) ---

const (
	distinctFineGran    = 64 << 10 // 64 KiB extent granularity (#118; was 1 MiB chunk-rounded)
	distinctCoarseGran  = 64 << 20
	distinctCoarseAbove = 1 << 40 // objects larger than 1 TiB use the coarse granularity
	distinctMaxObjs     = 1 << 20 // defensive cap on tracked objects
)

// objBits is a per-object touched-chunk bitmap at a fixed granularity.
type objBits struct {
	gran  int64
	words []uint64
	set   int64
}

func (o *objBits) mark(ci int64) {
	w := ci >> 6
	for int64(len(o.words)) <= w {
		o.words = append(o.words, 0)
	}
	b := uint64(1) << uint(ci&63)
	if o.words[w]&b == 0 {
		o.words[w] |= b
		o.set++
	}
}

// distinctReads aggregates per-object chunk bitmaps to report distinct bytes
// read across a mount. Bounded: each object's bitmap is O(size/gran) bits (and
// gran coarsens above 1 TiB), and the object count is capped.
type distinctReads struct {
	mu   sync.Mutex
	objs map[string]*objBits
}

func newDistinctReads() *distinctReads { return &distinctReads{objs: map[string]*objBits{}} }

func (d *distinctReads) mark(key string, off, length, size int64) {
	if length <= 0 || off < 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o := d.objs[key]
	if o == nil {
		if len(d.objs) >= distinctMaxObjs {
			return
		}
		gran := int64(distinctFineGran)
		if size > distinctCoarseAbove {
			gran = distinctCoarseGran
		}
		o = &objBits{gran: gran}
		d.objs[key] = o
	}
	end := off + length
	if size > 0 && end > size {
		end = size
	}
	if end <= off {
		return
	}
	for ci := off / o.gran; ci <= (end-1)/o.gran; ci++ {
		o.mark(ci)
	}
}

func (d *distinctReads) totalBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var total int64
	for _, o := range d.objs {
		total += o.set * o.gran
	}
	return total
}
