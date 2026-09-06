// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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
	ops           int
	s3Concurrency int
	maxReadahead  int64

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
			if key == "" {
				return fmt.Errorf("bench needs an object key: s3://bucket/key")
			}
			return runBench(cmd.Context(), cmd.OutOrStdout(), &f, bucket, key)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.pattern, "pattern", "seq", "access pattern: seq | rand4k | stride")
	fl.StringVar(&f.against, "against", "", "also run the identical pattern on this file path (e.g. a mountpoint-s3 mount)")
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "fill/readahead block size")
	fl.StringVar(&f.memCache, "mem-cache", "1GiB", "memory cache size")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk cache size (0 disables)")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory")
	fl.IntVar(&f.ops, "ops", 10000, "number of ops for rand4k/stride")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 64, "max concurrent S3 requests")
	fl.Int64Var(&f.maxReadahead, "max-readahead", 32, "max sequential readahead window in blocks")
	fl.BoolVar(&f.noSignRequest, "no-sign-request", false, "send anonymous requests (public buckets)")
	fl.BoolVar(&f.requesterPays, "requester-pays", false, "add the requester-pays header")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.region, "region", "", "bucket region")
	return cmd
}

// countingRecorder tallies S3 and prefetch activity for the cost estimate and
// the prefetch-is-driven assertion.
type countingRecorder struct {
	reqs           int64
	bytes          int64
	prefetchIssued int64
	prefetchHit    int64
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

type stats struct {
	mbps       float64
	iops       float64
	p50, p99   time.Duration
	ops        int
	bytes      int64
	s3Requests int64
	s3Bytes    int64
}

func runBench(ctx context.Context, out io.Writer, f *benchFlags, bucket, key string) error {
	blockSize, err := parseSize(f.blockSize)
	if err != nil {
		return err
	}
	memCache, _ := parseSize(f.memCache)
	diskCache, _ := parseSize(f.diskCache)

	client, err := newS3Client(ctx, s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSignRequest,
		RequesterPays: f.requesterPays, Endpoint: f.endpoint, PathStyle: f.pathStyle,
		Concurrency: f.s3Concurrency,
	})
	if err != nil {
		return err
	}
	head, err := client.HeadObject(ctx, key)
	if err != nil {
		return fmt.Errorf("head %s: %w", key, err)
	}

	// Build a one-object index and mount it so reads exercise the real FUSE
	// read path and its per-handle prefetcher (#36).
	ix := index.Build([]index.Entry{{
		Key: key, Size: head.Size, MTime: head.LastModified.UnixNano(), ETagHash: index.HashETag(head.ETag),
	}}, index.Options{Bucket: bucket})

	rec := &countingRecorder{}
	diskPath := f.diskPath
	if diskPath == "" {
		diskPath, _ = os.MkdirTemp("", "lith-bench-cache-")
		defer func() { _ = os.RemoveAll(diskPath) }()
	}
	bs, err := blockstore.New(client, blockstore.Config{
		Bucket: bucket, BlockSize: blockSize, MemCache: memCache,
		DiskCache: diskCache, DiskPath: diskPath, S3Concurrency: f.s3Concurrency, Recorder: rec,
	})
	if err != nil {
		return err
	}

	mnt, err := os.MkdirTemp("", "lith-bench-mnt-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(mnt) }()
	srv, err := fusefs.Mount(mnt, fusefs.Config{
		Index: ix, Store: bs, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MaxReadahead: f.maxReadahead,
	}, fusefs.MountOptions{FsName: "lith-bench"})
	if err != nil {
		return fmt.Errorf("bench mount: %w", err)
	}
	defer func() { _ = srv.Unmount() }()

	lithPath := filepath.Join(mnt, key)

	// lith: cold then warm.
	lithCold, err := benchFile(f.pattern, lithPath, head.Size, f.ops)
	if err != nil {
		return err
	}
	lithCold.s3Requests, lithCold.s3Bytes = atomic.LoadInt64(&rec.reqs), atomic.LoadInt64(&rec.bytes)
	prefetchAfterCold := atomic.LoadInt64(&rec.prefetchIssued)
	rBefore := atomic.LoadInt64(&rec.reqs)
	lithWarm, err := benchFile(f.pattern, lithPath, head.Size, f.ops)
	if err != nil {
		return err
	}
	lithWarm.s3Requests = atomic.LoadInt64(&rec.reqs) - rBefore

	cols := []string{"lith cold", "lith warm"}
	results := []stats{lithCold, lithWarm}

	if f.against != "" {
		ac, aerr := benchFile(f.pattern, f.against, head.Size, f.ops)
		if aerr != nil {
			return fmt.Errorf("--against %s: %w", f.against, aerr)
		}
		aw, aerr := benchFile(f.pattern, f.against, head.Size, f.ops)
		if aerr != nil {
			return err
		}
		cols = append(cols, "against cold", "against warm")
		results = append(results, ac, aw)
	}

	printBenchTable(out, f.pattern, key, head.Size, cols, results)

	// Assertion (#36): a sequential run must drive the prefetcher.
	if f.pattern == "seq" {
		if prefetchAfterCold == 0 {
			_, _ = fmt.Fprintln(out, "\nWARNING: prefetch count was 0 during seq — the prefetcher was not driven (see #36).")
		} else {
			_, _ = fmt.Fprintf(out, "\nprefetch issued during seq (cold): %d blocks (prefetcher is driven).\n", prefetchAfterCold)
		}
	}
	return nil
}

// benchFile runs a pattern against a file path using pread (ReadAt).
func benchFile(pattern, path string, size int64, ops int) (stats, error) {
	fd, err := os.Open(path)
	if err != nil {
		return stats{}, err
	}
	defer func() { _ = fd.Close() }()
	switch pattern {
	case "seq":
		return runSeq(fd, size), nil
	case "rand4k":
		return runRandom(fd, size, ops, 4096), nil
	case "stride":
		return runStride(fd, size, ops, 4096), nil
	default:
		return runSeq(fd, size), nil
	}
}

func runSeq(r io.ReaderAt, size int64) stats {
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var lat []time.Duration
	var total int64
	start := time.Now()
	for off := int64(0); off < size; off += chunk {
		n := int64(chunk)
		if off+n > size {
			n = size - off
		}
		t0 := time.Now()
		got, err := r.ReadAt(buf[:n], off)
		lat = append(lat, time.Since(t0))
		total += int64(got)
		if err != nil && err != io.EOF {
			break
		}
	}
	return summarize(lat, total, time.Since(start))
}

func runRandom(r io.ReaderAt, size int64, ops int, rsize int64) stats {
	buf := make([]byte, rsize)
	rng := rand.New(rand.NewSource(1))
	span := size - rsize
	if span <= 0 {
		span = 1
	}
	var lat []time.Duration
	var total int64
	start := time.Now()
	for i := 0; i < ops; i++ {
		off := rng.Int63n(span)
		t0 := time.Now()
		got, err := r.ReadAt(buf, off)
		lat = append(lat, time.Since(t0))
		total += int64(got)
		if err != nil && err != io.EOF {
			break
		}
	}
	return summarize(lat, total, time.Since(start))
}

func runStride(r io.ReaderAt, size int64, ops int, rsize int64) stats {
	const stride = 1 << 20
	buf := make([]byte, rsize)
	var lat []time.Duration
	var total int64
	off := int64(0)
	start := time.Now()
	for i := 0; i < ops; i++ {
		if off+rsize > size {
			off = 0
		}
		t0 := time.Now()
		got, err := r.ReadAt(buf, off)
		lat = append(lat, time.Since(t0))
		total += int64(got)
		if err != nil && err != io.EOF {
			break
		}
		off += stride
	}
	return summarize(lat, total, time.Since(start))
}

func summarize(lat []time.Duration, totalBytes int64, elapsed time.Duration) stats {
	s := stats{ops: len(lat), bytes: totalBytes}
	if elapsed > 0 {
		s.mbps = float64(totalBytes) / (1 << 20) / elapsed.Seconds()
		s.iops = float64(len(lat)) / elapsed.Seconds()
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		s.p50 = lat[len(lat)*50/100]
		s.p99 = lat[min(len(lat)-1, len(lat)*99/100)]
	}
	return s
}

func printBenchTable(out io.Writer, pattern, key string, size int64, cols []string, results []stats) {
	_, _ = fmt.Fprintf(out, "bench pattern=%s object=%s size=%d bytes\n\n", pattern, key, size)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "metric"
	for _, c := range cols {
		header += "\t" + c
	}
	_, _ = fmt.Fprintln(tw, header)
	row := func(name string, fn func(stats) string) {
		line := name
		for _, r := range results {
			line += "\t" + fn(r)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	row("MB/s", func(s stats) string { return fmt.Sprintf("%.1f", s.mbps) })
	row("IOPS", func(s stats) string { return fmt.Sprintf("%.0f", s.iops) })
	row("p50", func(s stats) string { return s.p50.String() })
	row("p99", func(s stats) string { return s.p99.String() })
	row("ops", func(s stats) string { return fmt.Sprintf("%d", s.ops) })
	row("S3 reqs", func(s stats) string { return fmt.Sprintf("%d", s.s3Requests) })
	row("S3 MB", func(s stats) string { return fmt.Sprintf("%.1f", float64(s.s3Bytes)/(1<<20)) })
	row("est. cost", func(s stats) string { return fmt.Sprintf("$%.6f", benchCost(s)) })
	_ = tw.Flush()
	_, _ = fmt.Fprintln(out, "\nCost: S3 GET at $0.40/1M requests + egress at $0.09/GB (0 within-region to EC2). S3 columns are lith-only.")
}

func benchCost(s stats) float64 {
	const getPer = 0.40 / 1e6
	const egressPerGB = 0.09
	return float64(s.s3Requests)*getPer + float64(s.s3Bytes)/1e9*egressPerGB
}
