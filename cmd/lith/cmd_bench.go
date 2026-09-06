// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
	"github.com/zeebo/xxh3"
)

type benchFlags struct {
	pattern   string
	against   string
	blockSize string
	memCache  string
	diskCache string
	diskPath  string
	ops       int

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
		Short: "Benchmark read patterns through lith (and optionally against another mount)",
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
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "block size")
	fl.StringVar(&f.memCache, "mem-cache", "1GiB", "memory cache size")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk cache size (0 disables)")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory")
	fl.IntVar(&f.ops, "ops", 10000, "number of ops for rand4k/stride")
	fl.BoolVar(&f.noSignRequest, "no-sign-request", false, "send anonymous requests (public buckets)")
	fl.BoolVar(&f.requesterPays, "requester-pays", false, "add the requester-pays header")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.region, "region", "", "bucket region")
	return cmd
}

// countingRecorder tallies S3 requests and bytes for the cost estimate.
type countingRecorder struct {
	reqs  int64
	bytes int64
}

func (c *countingRecorder) MemHit()  {}
func (c *countingRecorder) DiskHit() {}
func (c *countingRecorder) Miss()    {}
func (c *countingRecorder) S3Get(n int64, _ bool) {
	atomic.AddInt64(&c.reqs, 1)
	atomic.AddInt64(&c.bytes, n)
}
func (c *countingRecorder) StaleKey(string) {}
func (c *countingRecorder) PrefetchIssued() {}
func (c *countingRecorder) PrefetchHit()    {}

// readerAt is a seekable, sized reader for a bench engine.
type readerAt interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
}

// lithReader reads through the block store.
type lithReader struct {
	ctx  context.Context
	bs   *blockstore.BlockStore
	key  blockstore.Key
	size int64
}

func (r *lithReader) Size() int64 { return r.size }
func (r *lithReader) ReadAt(p []byte, off int64) (int, error) {
	data, err := r.bs.GetRange(r.ctx, r.key, off, int64(len(p)), r.size)
	if err != nil {
		return 0, err
	}
	return copy(p, data), nil
}

// fileReader reads a local file (e.g. a mountpoint-s3 mount).
type fileReader struct {
	f    *os.File
	size int64
}

func (r *fileReader) Size() int64                             { return r.size }
func (r *fileReader) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }

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
	})
	if err != nil {
		return err
	}
	head, err := client.HeadObject(ctx, key)
	if err != nil {
		return fmt.Errorf("head %s: %w", key, err)
	}

	newLith := func() (*lithReader, *countingRecorder, error) {
		rec := &countingRecorder{}
		bs, berr := blockstore.New(client, blockstore.Config{
			Bucket: bucket, BlockSize: blockSize, MemCache: memCache,
			DiskCache: diskCache, DiskPath: f.diskPath, Recorder: rec,
		})
		if berr != nil {
			return nil, nil, berr
		}
		return &lithReader{ctx: ctx, bs: bs, key: blockstore.Key{Key: key, ETagHash: xxh3.HashString(head.ETag)}, size: head.Size}, rec, nil
	}

	// lith: cold uses a fresh cache; warm reuses it.
	lr, rec, err := newLith()
	if err != nil {
		return err
	}
	lithCold := runPattern(f.pattern, lr, f.ops)
	lithCold.s3Requests, lithCold.s3Bytes = atomic.LoadInt64(&rec.reqs), atomic.LoadInt64(&rec.bytes)
	before := atomic.LoadInt64(&rec.reqs)
	lithWarm := runPattern(f.pattern, lr, f.ops)
	lithWarm.s3Requests = atomic.LoadInt64(&rec.reqs) - before
	lithWarm.s3Bytes = atomic.LoadInt64(&rec.bytes) - lithCold.s3Bytes

	cols := []string{"lith cold", "lith warm"}
	results := []stats{lithCold, lithWarm}

	if f.against != "" {
		af, aerr := os.Open(f.against)
		if aerr != nil {
			return fmt.Errorf("open --against %s: %w", f.against, aerr)
		}
		defer func() { _ = af.Close() }()
		fi, _ := af.Stat()
		fr := &fileReader{f: af, size: fi.Size()}
		againstCold := runPattern(f.pattern, fr, f.ops)
		againstWarm := runPattern(f.pattern, fr, f.ops)
		cols = append(cols, "against cold", "against warm")
		results = append(results, againstCold, againstWarm)
	}

	printBenchTable(out, f.pattern, key, head.Size, cols, results)
	return nil
}

// runPattern executes the named pattern against r and returns timing stats.
func runPattern(pattern string, r readerAt, ops int) stats {
	switch pattern {
	case "seq":
		return runSeq(r)
	case "rand4k":
		return runRandom(r, ops, 4096)
	case "stride":
		return runStride(r, ops, 4096)
	default:
		return runSeq(r)
	}
}

func runSeq(r readerAt) stats {
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	var lat []time.Duration
	var total int64
	start := time.Now()
	for off := int64(0); off < r.Size(); off += chunk {
		n := int64(chunk)
		if off+n > r.Size() {
			n = r.Size() - off
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

func runRandom(r readerAt, ops int, size int64) stats {
	buf := make([]byte, size)
	rng := rand.New(rand.NewSource(1))
	span := r.Size() - size
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

func runStride(r readerAt, ops int, size int64) stats {
	const stride = 1 << 20
	buf := make([]byte, size)
	var lat []time.Duration
	var total int64
	off := int64(0)
	start := time.Now()
	for i := 0; i < ops; i++ {
		if off+size > r.Size() {
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
	_, _ = fmt.Fprintln(out, "\nCost: S3 GET at $0.40/1M requests + egress at $0.09/GB (0 within-region to EC2).")
}

// benchCost estimates request + egress cost at published us-east-1 prices.
func benchCost(s stats) float64 {
	const getPer = 0.40 / 1e6
	const egressPerGB = 0.09
	return float64(s.s3Requests)*getPer + float64(s.s3Bytes)/1e9*egressPerGB
}
