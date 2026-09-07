// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	fusefs "github.com/scttfrdmn/lith/internal/fuse"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
)

type benchFlags struct {
	pattern       string
	against       string
	blockSize     string
	memCache      string
	diskCache     string
	diskPath      string
	cacheDir      string
	ops           int
	runs          int
	readers       int
	objects       string
	s3Concurrency int
	prefetchConc  int
	maxRange      string
	maxReadahead  int64
	diskWriters   int
	inflightBytes string

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
	fl.StringVar(&f.maxRange, "max-range", "64MiB", "max coalesced range GET size")
	fl.Int64Var(&f.maxReadahead, "max-readahead", 64, "max sequential readahead window in blocks")
	fl.IntVar(&f.diskWriters, "disk-writers", 4, "write-behind workers for the disk cache")
	fl.StringVar(&f.inflightBytes, "inflight-bytes", "", "max bytes in flight to S3 (default: 2 × NIC bandwidth × 100ms)")
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
func (c *countingRecorder) StaleKey(string) {}
func (c *countingRecorder) PrefetchIssued() { atomic.AddInt64(&c.prefetchIssued, 1) }
func (c *countingRecorder) PrefetchHit()    { atomic.AddInt64(&c.prefetchHit, 1) }
func (c *countingRecorder) UncoveredMiss()  { atomic.AddInt64(&c.uncovered, 1) }

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
	windowBytes := f.maxReadahead * blockSize

	client, err := newS3Client(ctx, s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSignRequest,
		RequesterPays: f.requesterPays, Endpoint: f.endpoint, PathStyle: f.pathStyle,
		Concurrency: f.s3Concurrency,
	})
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
		inflight, _ := computeInflightBytes(f.inflightBytes)
		bs, berr := blockstore.New(client, blockstore.Config{
			Bucket: bucket, BlockSize: blockSize, MemCache: memCache,
			DiskCache: diskCache, DiskPath: cacheDir, MaxRange: maxRange,
			S3Concurrency: f.s3Concurrency, PrefetchConcurrency: f.prefetchConc,
			DiskWriters: f.diskWriters, InflightBytes: inflight, Recorder: rec,
		})
		return bs, rec, berr
	}

	if f.readers > 0 {
		return runMultiReader(ctx, out, f, client, mkStore, cacheBase)
	}
	if key == "" {
		return fmt.Errorf("bench needs an object key: s3://bucket/key")
	}
	return runSingle(ctx, out, f, client, mkStore, cacheBase, bucket, key, blockSize, windowBytes)
}

// mountObjects builds an index over the given keys and mounts it, returning the
// mount dir, the server, the recorder, and a cleanup func.
func mountObjects(ctx context.Context, client s3client.API, bs *blockstore.BlockStore, bucket string, keys []string, maxReadahead int64) (string, *fusefs.Config, func(), error) {
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
	cfg := &fusefs.Config{Index: ix, Store: bs, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MaxReadahead: maxReadahead}
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

	for r := 0; r < f.runs; r++ {
		cacheDir := filepath.Join(cacheBase, fmt.Sprintf("lith-bench-cold-%d-%d", os.Getpid(), r))
		bs, rec, berr := mkStore(cacheDir)
		if berr != nil {
			return berr
		}
		mnt, _, cleanup, merr := mountObjects(ctx, client, bs, bucket, []string{key}, f.maxReadahead)
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
	mnt, _, cleanup, merr := mountObjects(ctx, client, bs, bucket, []string{key}, f.maxReadahead)
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
	_ = tw.Flush()
	if f.pattern == "seq" && prefetch == 0 {
		_, _ = fmt.Fprintln(out, "\nWARNING: prefetch count 0 during seq (see #36)")
	}
	return nil
}

func runMultiReader(ctx context.Context, out io.Writer, f *benchFlags, client s3client.API, mkStore func(string) (*blockstore.BlockStore, *countingRecorder, error), cacheBase string) error {
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
	mnt, _, cleanup, merr := mountObjects(ctx, client, bs, benchBucket, keys, f.maxReadahead)
	if merr != nil {
		return merr
	}
	defer func() { cleanup(); bs.Close(); _ = os.RemoveAll(cacheDir) }()

	_ = dropPageCache()
	litPaths := make([]string, len(keys))
	for i, k := range keys {
		litPaths[i] = filepath.Join(mnt, k)
	}
	litAgg, litPer := runReadersPaths(litPaths)

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
