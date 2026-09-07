// SPDX-License-Identifier: Apache-2.0

// Command lith-s3bench isolates the S3 client/transport from lith's cache,
// prefetch, and FUSE layers. N workers issue back-to-back ranged GetObject
// requests over a set of keys, reading each body fully into a preallocated
// buffer and discarding it. It answers one question: what aggregate throughput
// can the Go aws-sdk-go-v2 client sustain on this box, independent of lith?
//
// The transport is tuned identically to internal/s3client (design §4.5): no
// HTTP/2, unbounded MaxConnsPerHost, a large idle-conn pool, 90s idle timeout.
// This is diagnostic tooling, not part of the shipped filesystem.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// tuneTransport mirrors internal/s3client.tuneTransport (design §4.5) so the
// harness exercises the exact transport lith uses. readBuffer, when > 0, sets
// http.Transport.ReadBufferSize (0 = Go default 4 KiB) — a diagnostic knob.
func tuneTransport(t *http.Transport, concurrency, readBuffer int) {
	if concurrency <= 0 {
		concurrency = 64
	}
	t.Proxy = http.ProxyFromEnvironment
	t.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	t.ForceAttemptHTTP2 = false
	t.MaxConnsPerHost = 0
	t.MaxIdleConns = concurrency * 2
	t.MaxIdleConnsPerHost = concurrency
	t.IdleConnTimeout = 90 * time.Second
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ExpectContinueTimeout = time.Second
	if readBuffer > 0 {
		t.ReadBufferSize = readBuffer
	}
}

type rangeReq struct {
	keyIdx int
	off    int64
	length int64
}

func main() {
	var (
		bucket     = flag.String("bucket", "1000genomes", "S3 bucket")
		region     = flag.String("region", "us-east-1", "bucket region")
		keysCSV    = flag.String("keys", "", "comma-separated keys (required)")
		workers    = flag.Int("workers", 64, "concurrent workers")
		pool       = flag.Int("pool", 0, "transport idle-conn pool size (0 = workers)")
		partStr    = flag.Int64("part", 8<<20, "range GET size in bytes")
		dur        = flag.Duration("duration", 20*time.Second, "measurement window")
		noSign     = flag.Bool("no-sign-request", true, "anonymous requests")
		useHTTP    = flag.Bool("http", false, "use http:// (isolates TLS cost; public buckets only)")
		readBuffer = flag.Int("read-buffer", 0, "http.Transport.ReadBufferSize in bytes (0 = default)")
	)
	flag.Parse()

	if *keysCSV == "" {
		fmt.Fprintln(os.Stderr, "need --keys")
		os.Exit(2)
	}
	keys := strings.Split(*keysCSV, ",")
	poolSize := *pool
	if poolSize == 0 {
		poolSize = *workers
	}

	ctx := context.Background()

	// Build the client exactly like internal/s3client, plus the harness knobs.
	httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		tuneTransport(t, poolSize, *readBuffer)
	})
	loadOpts := []func(*config.LoadOptions) error{
		config.WithHTTPClient(httpClient),
		config.WithRegion(*region),
	}
	if *noSign {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	s3Opts := func(o *s3.Options) {
		if *useHTTP {
			o.BaseEndpoint = aws.String("http://s3." + *region + ".amazonaws.com")
			o.UsePathStyle = true
		}
	}
	cl := s3.NewFromConfig(awsCfg, s3Opts)

	// HeadObject each key for its size, then build the flat range list.
	var reqs []rangeReq
	var totalObjBytes int64
	for i, k := range keys {
		h, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(k)})
		if err != nil {
			fmt.Fprintf(os.Stderr, "head %s: %v\n", k, err)
			os.Exit(1)
		}
		size := aws.ToInt64(h.ContentLength)
		totalObjBytes += size
		for off := int64(0); off < size; off += *partStr {
			l := *partStr
			if off+l > size {
				l = size - off
			}
			reqs = append(reqs, rangeReq{keyIdx: i, off: off, length: l})
		}
	}
	fmt.Printf("# %d keys, %.1f GiB total, %d ranges of %d MiB, %d workers, pool=%d, http=%v, read-buffer=%d\n",
		len(keys), float64(totalObjBytes)/(1<<30), len(reqs), *partStr>>20, *workers, poolSize, *useHTTP, *readBuffer)

	// Sample established connection count while running.
	scheme := "443"
	if *useHTTP {
		scheme = "80"
	}
	stopSample := make(chan struct{})
	var maxConns int32
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-t.C:
				if n := sampleConns(scheme); n > int(atomic.LoadInt32(&maxConns)) {
					atomic.StoreInt32(&maxConns, int32(n))
				}
			}
		}
	}()

	var (
		cursor    atomic.Int64
		bytesRead atomic.Int64
		lat       = make([][]time.Duration, *workers)
		wg        sync.WaitGroup
	)
	deadline := time.Now().Add(*dur)
	rusStart := getCPU()
	wallStart := time.Now()

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, *partStr)
			var mine []time.Duration
			for time.Now().Before(deadline) {
				idx := int(cursor.Add(1)-1) % len(reqs)
				r := reqs[idx]
				t0 := time.Now()
				out, err := cl.GetObject(ctx, &s3.GetObjectInput{
					Bucket: bucket,
					Key:    aws.String(keys[r.keyIdx]),
					Range:  aws.String(fmt.Sprintf("bytes=%d-%d", r.off, r.off+r.length-1)),
				})
				if err != nil {
					fmt.Fprintln(os.Stderr, "get:", err)
					continue
				}
				n, err := io.ReadFull(out.Body, buf[:r.length])
				_ = out.Body.Close()
				if err != nil && err != io.ErrUnexpectedEOF {
					fmt.Fprintln(os.Stderr, "read:", err)
					continue
				}
				bytesRead.Add(int64(n))
				mine = append(mine, time.Since(t0))
			}
			lat[w] = mine
		}(w)
	}
	wg.Wait()
	wall := time.Since(wallStart)
	close(stopSample)
	rusEnd := getCPU()

	// Merge latencies.
	var all []time.Duration
	for _, m := range lat {
		all = append(all, m...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		i := int(p / 100 * float64(len(all)))
		if i >= len(all) {
			i = len(all) - 1
		}
		return all[i]
	}

	mbps := float64(bytesRead.Load()) / (1 << 20) / wall.Seconds()
	cpuSec := rusEnd - rusStart
	ncpu := float64(runtime.NumCPU())
	fmt.Printf("RESULT workers=%d part=%dMiB http=%v rbuf=%d  agg=%.0f MB/s  reqs=%d  p50=%.1fms p99=%.1fms  maxconns=%d  cpu=%.1fs (%.0f%% of %g cores)\n",
		*workers, *partStr>>20, *useHTTP, *readBuffer,
		mbps, len(all), float64(pct(50).Microseconds())/1000, float64(pct(99).Microseconds())/1000,
		atomic.LoadInt32(&maxConns), cpuSec, cpuSec/wall.Seconds()/ncpu*100, ncpu)
}

func sampleConns(port string) int {
	out, err := exec.Command("ss", "-tn", "state", "established").CombinedOutput()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, ":"+port) {
			n++
		}
	}
	return n
}

func getCPU() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	u := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	s := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return (u + s).Seconds()
}
