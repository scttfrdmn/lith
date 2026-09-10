// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	fusefs "github.com/scttfrdmn/lith/internal/fuse"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
)

type benchFlags struct {
	pattern        string
	against        string
	blockSize      string
	memCache       string
	diskCache      string
	diskPath       string
	cacheDir       string
	ops            int
	runs           int
	readers        int
	objects        string
	s3Concurrency  int
	prefetchConc   int
	prefetchBudget string
	maxRange       string
	maxReadahead   int64
	diskWriters    int
	inflightBytes  string
	timelineCSV    string

	noSignRequest bool
	requesterPays bool
	endpoint      string
	pathStyle     bool
	region        string
}

func newBenchCmd() *cobra.Command {
	var f benchFlags
	cmd := &cobra.Command{
		Use:   "bench s3://bucket/key",
		Short: "Benchmark read patterns through a lith mount (and optionally against another mount)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, key, err := parseS3URL(args[0])
			if err != nil {
				return err
			}
			return runBench(cmd.Context(), cmd.OutOrStdout(), &f, bucket, key)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.pattern, "pattern", "seq", "access pattern: seq | rand4k | stride")
	fl.StringVar(&f.against, "against", "", "compare against this path (a file for single-object, a mount dir for --readers)")
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "fill/readahead block size")
	fl.StringVar(&f.memCache, "mem-cache", "1GiB", "memory cache size")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk cache size (0 disables)")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory")
	fl.StringVar(&f.cacheDir, "cache-dir", "", "throwaway base dir for per-run cold caches (default $TMPDIR)")
	fl.IntVar(&f.ops, "ops", 10000, "number of ops for rand4k/stride")
	fl.IntVar(&f.runs, "runs", 1, "number of cold runs (median reported); a warm run follows")
	fl.IntVar(&f.readers, "readers", 0, "concurrent readers (multi-object mode); requires --objects")
	fl.StringVar(&f.objects, "objects", "", "comma-separated keys for --readers mode")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 128, "max concurrent S3 requests")
	fl.IntVar(&f.prefetchConc, "prefetch-concurrency", 0, "max concurrent prefetch fills (0 = --s3-concurrency)")
	fl.StringVar(&f.prefetchBudget, "prefetch-budget", "", "max bytes of un-demanded prefetch (default: 50% of --mem-cache)")
	fl.StringVar(&f.maxRange, "max-range", "64MiB", "max coalesced range GET size")
	fl.Int64Var(&f.maxReadahead, "max-readahead", 0, "max sequential readahead window in blocks (0 = 1.5x the bandwidth-delay product, inflight-bytes/block; the 1.5x is empirical, measured on c8gd.16xlarge)")
	fl.IntVar(&f.diskWriters, "disk-writers", 4, "write-behind workers for the disk cache")
	fl.StringVar(&f.inflightBytes, "inflight-bytes", "", "max bytes in flight to S3 (default: 2 × NIC bandwidth × 100ms)")
	fl.StringVar(&f.timelineCSV, "timeline-csv", "", "in --readers mode, write a per-second per-reader MB/s timeline to this CSV")
	fl.BoolVar(&f.noSignRequest, "no-sign-request", false, "send anonymous requests (public buckets)")
	fl.BoolVar(&f.requesterPays, "requester-pays", false, "add the requester-pays header")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.region, "region", "", "bucket region")
	return cmd
}

// countingRecorder tallies S3 and prefetch activity.
type countingRecorder struct {
	reqs           int64
	bytes          int64
	prefetchIssued int64
	prefetchHit    int64
	uncovered      int64
	evictedUnread  int64

	waitMu    sync.Mutex
	waitNanos []int64 // prefetch-semaphore wait durations (ns)
}

func (c *countingRecorder) MemHit()        {}
func (c *countingRecorder) DiskHit()       {}
func (c *countingRecorder) Miss()          {}
func (c *countingRecorder) StartInflight() {}
func (c *countingRecorder) EndInflight()   {}
func (c *countingRecorder) S3Get(n int64, _ bool) {
	atomic.AddInt64(&c.reqs, 1)
	atomic.AddInt64(&c.bytes, n)
}
func (c *countingRecorder) StaleKey(string)        {}
func (c *countingRecorder) PrefetchIssued()        { atomic.AddInt64(&c.prefetchIssued, 1) }
func (c *countingRecorder) PrefetchHit()           { atomic.AddInt64(&c.prefetchHit, 1) }
func (c *countingRecorder) UncoveredMiss()         { atomic.AddInt64(&c.uncovered, 1) }
func (c *countingRecorder) PrefetchEvictedUnread() { atomic.AddInt64(&c.evictedUnread, 1) }
func (c *countingRecorder) PrefetchWait(d time.Duration) {
	c.waitMu.Lock()
	c.waitNanos = append(c.waitNanos, d.Nanoseconds())
	c.waitMu.Unlock()
}

// waitP99 returns the p99 prefetch-semaphore wait time.
func (c *countingRecorder) waitP99() time.Duration {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	if len(c.waitNanos) == 0 {
		return 0
	}
	s := append([]int64(nil), c.waitNanos...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(0.99 * float64(len(s)))
	if i >= len(s) {
		i = len(s) - 1
	}
	return time.Duration(s[i])
}

// runResult captures one pattern run.
type runResult struct {
	mbps       float64
	ttfb       time.Duration
	all        []time.Duration
	postWindow []time.Duration
}

func runBench(ctx context.Context, out io.Writer, f *benchFlags, bucket, key string) error {
	blockSize, err := parseSize(f.blockSize)
	if err != nil {
		return err
	}
	memCache, _ := parseSize(f.memCache)
	diskCache, _ := parseSize(f.diskCache)
	maxRange, err := parseSize(f.maxRange)
	if err != nil {
		return err
	}
	var prefetchBudget int64
	if f.prefetchBudget != "" {
		if prefetchBudget, err = parseSize(f.prefetchBudget); err != nil {
			return err
		}
	}
	// Default the single-handle readahead window to the bandwidth-delay product
	// (inflight-bytes / block) so one reader can fill the NIC on a fat pipe (#56),
	// sized from the NIC baseline (#79).
	nicBaseline := resolveNIC(ctx, os.TempDir(), 0).BaselineGbps
	benchInflight, _ := computeInflightBytes(f.inflightBytes, nicBaseline)
	f.maxReadahead = effectiveReadahead(f.maxReadahead, benchInflight, blockSize)
	windowBytes := f.maxReadahead * blockSize

	// In --readers mode, count every HTTP attempt by status so the report can
	// separate 503s/retries (the #49 bimodality investigation).
	var s3status *statusCounter
	cfg := s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSignRequest,
		RequesterPays: f.requesterPays, Endpoint: f.endpoint, PathStyle: f.pathStyle,
		Concurrency: f.s3Concurrency,
	}
	if f.readers > 0 {
		s3status = &statusCounter{}
		cfg.TransportWrap = s3status.wrap
	}
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return err
	}

	cacheBase := f.cacheDir
	if cacheBase == "" {
		cacheBase = os.TempDir()
	}
	benchBucket = bucket

	mkStore := func(cacheDir string) (*blockstore.BlockStore, *countingRecorder, error) {
		rec := &countingRecorder{}
		inflight, _ := computeInflightBytes(f.inflightBytes, nicBaseline)
		bs, berr := blockstore.New(client, blockstore.Config{
			Bucket: bucket, BlockSize: blockSize, MemCache: memCache,
			DiskCache: diskCache, DiskPath: cacheDir, MaxRange: maxRange,
			S3Concurrency: f.s3Concurrency, PrefetchConcurrency: f.prefetchConc,
			PrefetchBudget: prefetchBudget,
			DiskWriters:    f.diskWriters, InflightBytes: inflight, Recorder: rec,
		})
		return bs, rec, berr
	}

	if f.readers > 0 {
		return runMultiReader(ctx, out, f, client, mkStore, cacheBase, s3status)
	}
	if key == "" {
		return fmt.Errorf("bench needs an object key: s3://bucket/key")
	}
	return runSingle(ctx, out, f, client, mkStore, cacheBase, bucket, key, blockSize, windowBytes)
}

// mountObjects builds an index over the given keys and mounts it, returning the
// mount dir, the server, the recorder, and a cleanup func.
func mountObjects(ctx context.Context, client s3client.API, bs *blockstore.BlockStore, bucket string, keys []string, maxReadahead int64, stats *fusefs.PrefetchStats, met *metrics.Metrics) (string, *fusefs.Config, func(), error) {
	entries := make([]index.Entry, 0, len(keys))
	for _, k := range keys {
		h, err := client.HeadObject(ctx, k)
		if err != nil {
			return "", nil, nil, fmt.Errorf("head %s: %w", k, err)
		}
		entries = append(entries, index.Entry{Key: k, Size: h.Size, MTime: h.LastModified.UnixNano(), ETagHash: index.HashETag(h.ETag)})
	}
	ix := index.Build(entries, index.Options{Bucket: bucket})
	mnt, err := os.MkdirTemp("", "lith-bench-mnt-")
	if err != nil {
		return "", nil, nil, err
	}
	cfg := &fusefs.Config{Index: ix, Store: bs, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MaxReadahead: maxReadahead, PrefetchStats: stats, Metrics: met}
	srv, err := fusefs.Mount(mnt, *cfg, fusefs.MountOptions{FsName: "lith-bench"})
	if err != nil {
		_ = os.RemoveAll(mnt)
		return "", nil, nil, fmt.Errorf("bench mount: %w", err)
	}
	cleanup := func() { _ = srv.Unmount(); _ = os.RemoveAll(mnt) }
	return mnt, cfg, cleanup, nil
}

func runSingle(ctx context.Context, out io.Writer, f *benchFlags, client s3client.API, mkStore func(string) (*blockstore.BlockStore, *countingRecorder, error), cacheBase, bucket, key string, blockSize, windowBytes int64) error {
	head, err := client.HeadObject(ctx, key)
	if err != nil {
		return fmt.Errorf("head %s: %w", key, err)
	}
	size := head.Size

	var coldMBps []float64
	var aggAll, aggPost []time.Duration
	var ttfb time.Duration
	var s3reqs, s3bytes, prefetch, uncovered int64
	var lastMet *metrics.Metrics

	for r := 0; r < f.runs; r++ {
		cacheDir := filepath.Join(cacheBase, fmt.Sprintf("lith-bench-cold-%d-%d", os.Getpid(), r))
		bs, rec, berr := mkStore(cacheDir)
		if berr != nil {
			return berr
		}
		met := metrics.New()
		lastMet = met
		mnt, _, cleanup, merr := mountObjects(ctx, client, bs, bucket, []string{key}, f.maxReadahead, nil, met)
		if merr != nil {
			return merr
		}
		_ = dropPageCache()
		res := benchOnce(filepath.Join(mnt, key), f.pattern, size, f.ops, windowBytes)
		cleanup()
		bs.Close()
		_ = os.RemoveAll(cacheDir)

		coldMBps = append(coldMBps, res.mbps)
		aggAll = append(aggAll, res.all...)
		aggPost = append(aggPost, res.postWindow...)
		if r == 0 {
			ttfb = res.ttfb
		}
		s3reqs, s3bytes = atomic.LoadInt64(&rec.reqs), atomic.LoadInt64(&rec.bytes)
		prefetch, uncovered = atomic.LoadInt64(&rec.prefetchIssued), atomic.LoadInt64(&rec.uncovered)
	}

	// Warm: one mount, run twice (first fills the cache, second is measured).
	warmCache := filepath.Join(cacheBase, fmt.Sprintf("lith-bench-warm-%d", os.Getpid()))
	bs, _, berr := mkStore(warmCache)
	if berr != nil {
		return berr
	}
	mnt, _, cleanup, merr := mountObjects(ctx, client, bs, bucket, []string{key}, f.maxReadahead, nil, nil)
	if merr != nil {
		return merr
	}
	_ = benchOnce(filepath.Join(mnt, key), f.pattern, size, f.ops, windowBytes) // warm the cache
	warm := benchOnce(filepath.Join(mnt, key), f.pattern, size, f.ops, windowBytes)
	cleanup()
	bs.Close()
	_ = os.RemoveAll(warmCache)

	// against
	var againstCold []float64
	var againstWarm float64
	if f.against != "" {
		for r := 0; r < f.runs; r++ {
			_ = dropPageCache()
			againstCold = append(againstCold, benchOnce(f.against, f.pattern, size, f.ops, windowBytes).mbps)
		}
		againstWarm = benchOnce(f.against, f.pattern, size, f.ops, windowBytes).mbps
	}

	// Report.
	_, _ = fmt.Fprintf(out, "bench pattern=%s object=%s size=%d runs=%d window=%dMiB\n\n", f.pattern, key, size, f.runs, windowBytes>>20)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	pr := func(a ...string) { _, _ = fmt.Fprintln(tw, strings.Join(a, "\t")) }
	pr("metric", "lith", "against")
	pr("cold MB/s (min/med/max)",
		fmt.Sprintf("%.0f / %.0f / %.0f", minFloat(coldMBps), medianFloat(coldMBps), maxFloat(coldMBps)),
		againstCol(f.against != "", againstCold))
	pr("warm MB/s", fmt.Sprintf("%.0f", warm.mbps), floatOrNA(f.against != "", againstWarm))
	pr("p50/p99/p99.9 all",
		lat3(percentile(aggAll, 50), percentile(aggAll, 99), percentile(aggAll, 99.9)), "-")
	pr("p50/p99/p99.9 post-window",
		lat3(percentile(aggPost, 50), percentile(aggPost, 99), percentile(aggPost, 99.9)), "-")
	pr("TTFB (open->first read)", ttfb.String(), "-")
	pr("S3 reqs / MB (lith, last cold)", fmt.Sprintf("%d / %.0f", s3reqs, float64(s3bytes)/(1<<20)), "-")
	pr("prefetch issued / uncovered", fmt.Sprintf("%d / %d", prefetch, uncovered), "-")
	if n, sum := lastMet.ReadSizeStats(); n > 0 {
		pr("reads / mean size (last cold)", fmt.Sprintf("%d / %.0f KiB", n, float64(sum)/float64(n)/1024), "-")
	}
	pr("distinct bytes read (last cold)", fmt.Sprintf("%.0f MiB", float64(lastMet.DistinctBytesRead())/(1<<20)), "-")
	_ = tw.Flush()
	if f.pattern == "seq" && prefetch == 0 {
		_, _ = fmt.Fprintln(out, "\nWARNING: prefetch count 0 during seq (see #36)")
	}
	return nil
}

func runMultiReader(ctx context.Context, out io.Writer, f *benchFlags, client s3client.API, mkStore func(string) (*blockstore.BlockStore, *countingRecorder, error), cacheBase string, s3status *statusCounter) error {
	if f.objects == "" {
		return fmt.Errorf("--readers requires --objects (comma-separated keys)")
	}
	keys := strings.Split(f.objects, ",")
	for i := range keys {
		keys[i] = strings.TrimSpace(keys[i])
	}
	cacheDir := filepath.Join(cacheBase, fmt.Sprintf("lith-bench-mr-%d", os.Getpid()))
	bs, rec, berr := mkStore(cacheDir)
	if berr != nil {
		return berr
	}
	pfStats := &fusefs.PrefetchStats{}
	mnt, _, cleanup, merr := mountObjects(ctx, client, bs, benchBucket, keys, f.maxReadahead, pfStats, nil)
	if merr != nil {
		return merr
	}
	defer func() { cleanup(); bs.Close(); _ = os.RemoveAll(cacheDir) }()

	_ = dropPageCache()
	litPaths := make([]string, len(keys))
	for i, k := range keys {
		litPaths[i] = filepath.Join(mnt, k)
	}
	litAgg, litPer, tl := runReadersTimeline(litPaths)
	time.Sleep(300 * time.Millisecond) // let FUSE Release fire so PrefetchStats populate

	if f.timelineCSV != "" {
		if werr := tl.writeCSV(f.timelineCSV); werr != nil {
			_, _ = fmt.Fprintf(out, "timeline CSV write failed: %v\n", werr)
		} else {
			_, _ = fmt.Fprintf(out, "per-reader timeline written to %s\n", f.timelineCSV)
		}
	}

	_, _ = fmt.Fprintf(out, "bench readers=%d objects=%d (cold)\n\n", len(keys), len(keys))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "metric\tlith\tagainst")
	var againstAgg float64
	var againstPer []float64
	if f.against != "" {
		_ = dropPageCache()
		apaths := make([]string, len(keys))
		for i, k := range keys {
			apaths[i] = filepath.Join(f.against, k)
		}
		againstAgg, againstPer = runReadersPaths(apaths)
	}
	_, _ = fmt.Fprintf(tw, "aggregate MB/s\t%.0f\t%s\n", litAgg, floatOrNA(f.against != "", againstAgg))
	_, _ = fmt.Fprintf(tw, "per-reader MB/s (min/med)\t%.0f / %.0f\t%s\n",
		minFloat(litPer), medianFloat(litPer), perReaderCol(f.against != "", againstPer))
	_, _ = fmt.Fprintf(tw, "S3 requests\t%d\t%s\n", atomic.LoadInt64(&rec.reqs), naIf(f.against != ""))
	// #49 instrumentation.
	if s3status != nil {
		_, _ = fmt.Fprintf(tw, "S3 attempts (2xx/503/other-5xx/4xx)\t%s\t-\n", s3status.summary())
	}
	_, _ = fmt.Fprintf(tw, "conn ramp :443 (t=1/2/3s, max)\t%s\t-\n", tl.connRamp())
	_, _ = fmt.Fprintf(tw, "prefetch sem wait p99\t%s\t-\n", rec.waitP99())
	_, _ = fmt.Fprintf(tw, "prefetch issued / uncovered\t%d / %d\t-\n",
		atomic.LoadInt64(&rec.prefetchIssued), atomic.LoadInt64(&rec.uncovered))
	_, _ = fmt.Fprintf(tw, "prefetch evicted-unread (thrash)\t%d\t-\n", atomic.LoadInt64(&rec.evictedUnread))
	halvings, resets, pw := pfStats.Snapshot()
	sort.Slice(pw, func(i, j int) bool { return pw[i] < pw[j] })
	_, _ = fmt.Fprintf(tw, "prefetch halved / reset-random\t%d / %d\t-\n", halvings, resets)
	_, _ = fmt.Fprintf(tw, "prefetch peak windows\t%v\t-\n", pw)
	_ = tw.Flush()
	return nil
}

// benchBucket is set by runBench so multi-reader mode can build its index.
var benchBucket string

func runReadersPaths(paths []string) (float64, []float64) {
	per := make([]float64, len(paths))
	start := time.Now()
	var wg sync.WaitGroup
	var totalBytes int64
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			fi, err := os.Stat(p)
			if err != nil {
				return
			}
			res := benchOnce(p, "seq", fi.Size(), 0, 1<<62)
			per[i] = res.mbps
			atomic.AddInt64(&totalBytes, fi.Size())
		}(i, p)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	agg := 0.0
	if elapsed > 0 {
		agg = float64(atomic.LoadInt64(&totalBytes)) / (1 << 20) / elapsed
	}
	return agg, per
}

// statusCounter wraps an http.RoundTripper and tallies every HTTP attempt by
// status class (including SDK retries, since each attempt is one RoundTrip),
// so the #49 report can separate 503 throttles from clean responses.
type statusCounter struct {
	ok2xx    int64
	throttle int64 // HTTP 503
	other5xx int64
	err4xx   int64
	rt       http.RoundTripper
}

func (s *statusCounter) wrap(rt http.RoundTripper) http.RoundTripper {
	s.rt = rt
	return s
}

func (s *statusCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.rt.RoundTrip(r)
	if err != nil || resp == nil {
		return resp, err
	}
	switch {
	case resp.StatusCode == 503:
		atomic.AddInt64(&s.throttle, 1)
	case resp.StatusCode >= 500:
		atomic.AddInt64(&s.other5xx, 1)
	case resp.StatusCode >= 400:
		atomic.AddInt64(&s.err4xx, 1)
	default:
		atomic.AddInt64(&s.ok2xx, 1)
	}
	return resp, err
}

func (s *statusCounter) summary() string {
	return fmt.Sprintf("%d / %d / %d / %d",
		atomic.LoadInt64(&s.ok2xx), atomic.LoadInt64(&s.throttle),
		atomic.LoadInt64(&s.other5xx), atomic.LoadInt64(&s.err4xx))
}

// timeline holds per-second per-reader throughput and :443 connection counts
// sampled during a multi-reader run.
type timeline struct {
	perSec [][]float64 // [second][reader] MB/s during that second
	conns  []int       // established :443 connections at each second
}

func (t *timeline) writeCSV(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprint(f, "second,conns443")
	if len(t.perSec) > 0 {
		for i := range t.perSec[0] {
			_, _ = fmt.Fprintf(f, ",reader%d_MBps", i)
		}
	}
	_, _ = fmt.Fprintln(f)
	for s, row := range t.perSec {
		_, _ = fmt.Fprintf(f, "%d,%d", s+1, t.conns[s])
		for _, v := range row {
			_, _ = fmt.Fprintf(f, ",%.0f", v)
		}
		_, _ = fmt.Fprintln(f)
	}
	return nil
}

func (t *timeline) connRamp() string {
	at := func(sec int) int {
		if sec-1 >= 0 && sec-1 < len(t.conns) {
			return t.conns[sec-1]
		}
		return 0
	}
	mx := 0
	for _, c := range t.conns {
		if c > mx {
			mx = c
		}
	}
	return fmt.Sprintf("%d/%d/%d, max %d", at(1), at(2), at(3), mx)
}

// runReadersTimeline is runReadersPaths plus a per-second sampler: it records
// each reader's MB/s and the :443 connection count every second (#49).
func runReadersTimeline(paths []string) (float64, []float64, *timeline) {
	n := len(paths)
	prog := make([]atomic.Int64, n) // cumulative bytes per reader
	per := make([]float64, n)
	sizes := make([]int64, n)
	tl := &timeline{}
	done := make(chan struct{})

	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		last := make([]int64, n)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				row := make([]float64, n)
				for i := 0; i < n; i++ {
					cur := prog[i].Load()
					row[i] = float64(cur-last[i]) / (1 << 20) // MB in 1 s == MB/s
					last[i] = cur
				}
				tl.perSec = append(tl.perSec, row)
				tl.conns = append(tl.conns, sampleConns443())
			}
		}
	}()

	start := time.Now()
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			fi, err := os.Stat(p)
			if err != nil {
				return
			}
			sizes[i] = fi.Size()
			per[i] = readSeqProgress(p, fi.Size(), &prog[i])
		}(i, p)
	}
	wg.Wait()
	close(done)
	samplerWG.Wait()

	elapsed := time.Since(start).Seconds()
	var total int64
	for _, s := range sizes {
		total += s
	}
	agg := 0.0
	if elapsed > 0 {
		agg = float64(total) / (1 << 20) / elapsed
	}
	return agg, per, tl
}

// readSeqProgress reads path sequentially in 1 MiB preads, publishing
// cumulative bytes to prog for the per-second sampler.
func readSeqProgress(path string, size int64, prog *atomic.Int64) float64 {
	fd, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = fd.Close() }()
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	start := time.Now()
	var total int64
	for off := int64(0); off < size; off += chunk {
		nn := int64(chunk)
		if off+nn > size {
			nn = size - off
		}
		got, e := fd.ReadAt(buf[:nn], off)
		total += int64(got)
		prog.Add(int64(got))
		if e != nil && e != io.EOF {
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed > 0 {
		return float64(total) / (1 << 20) / elapsed
	}
	return 0
}

// sampleConns443 counts established connections to :443 via ss.
func sampleConns443() int {
	// Resolve ss against a fixed trusted dir list (not $PATH) so a poisoned PATH
	// under `sudo` can't run an attacker binary (finding F5); ss is best-effort,
	// so a missing binary just samples 0.
	bin, err := trustedExecPath("ss")
	if err != nil {
		return 0
	}
	out, err := exec.Command(bin, "-tn", "state", "established").CombinedOutput()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ":443") {
			n++
		}
	}
	return n
}

// benchOnce runs a pattern over a file via pread, returning throughput, TTFB,
// and per-read latencies split at windowBytes.
func benchOnce(path, pattern string, size int64, ops int, windowBytes int64) runResult {
	openStart := time.Now()
	fd, err := os.Open(path)
	if err != nil {
		return runResult{}
	}
	defer func() { _ = fd.Close() }()

	var res runResult
	var total int64
	first := true
	record := func(off int64, lat time.Duration) {
		if first {
			res.ttfb = time.Since(openStart)
			first = false
		}
		res.all = append(res.all, lat)
		if off >= windowBytes {
			res.postWindow = append(res.postWindow, lat)
		}
	}

	start := time.Now()
	switch pattern {
	case "rand4k":
		buf := make([]byte, 4096)
		rng := rand.New(rand.NewSource(1))
		span := size - 4096
		if span <= 0 {
			span = 1
		}
		for i := 0; i < ops; i++ {
			off := rng.Int63n(span)
			t0 := time.Now()
			n, e := fd.ReadAt(buf, off)
			record(off, time.Since(t0))
			total += int64(n)
			if e != nil && e != io.EOF {
				break
			}
		}
	case "stride":
		const stride = 1 << 20
		buf := make([]byte, 4096)
		off := int64(0)
		for i := 0; i < ops; i++ {
			if off+4096 > size {
				off = 0
			}
			t0 := time.Now()
			n, e := fd.ReadAt(buf, off)
			record(off, time.Since(t0))
			total += int64(n)
			if e != nil && e != io.EOF {
				break
			}
			off += stride
		}
	default: // seq
		const chunk = 1 << 20
		buf := make([]byte, chunk)
		for off := int64(0); off < size; off += chunk {
			n := int64(chunk)
			if off+n > size {
				n = size - off
			}
			t0 := time.Now()
			got, e := fd.ReadAt(buf[:n], off)
			record(off, time.Since(t0))
			total += int64(got)
			if e != nil && e != io.EOF {
				break
			}
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed > 0 {
		res.mbps = float64(total) / (1 << 20) / elapsed
	}
	return res
}

// dropPageCache best-effort drops the OS page cache (needs root; run bench under
// sudo on the devbox for true-cold --against runs).
func dropPageCache() error {
	f, err := os.OpenFile("/proc/sys/vm/drop_caches", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("3\n")
	return err
}

func lat3(a, b, c time.Duration) string { return fmt.Sprintf("%v / %v / %v", a, b, c) }
func floatOrNA(has bool, v float64) string {
	if !has {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}
func naIf(has bool) string {
	if has {
		return "see mount-s3 --log-metrics"
	}
	return "-"
}
func againstCol(has bool, vals []float64) string {
	if !has {
		return "-"
	}
	return fmt.Sprintf("%.0f / %.0f / %.0f", minFloat(vals), medianFloat(vals), maxFloat(vals))
}
func perReaderCol(has bool, vals []float64) string {
	if !has {
		return "-"
	}
	return fmt.Sprintf("%.0f / %.0f", minFloat(vals), medianFloat(vals))
}
