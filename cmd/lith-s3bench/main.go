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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/scttfrdmn/lith/internal/s3client"
)

// tuneTransport applies lith's exact transport tuning (via the shared
// s3client.TuneTransport, design §4.5) and layers on the diagnostic-only
// readBuffer knob: when > 0 it sets http.Transport.ReadBufferSize (0 = Go
// default 4 KiB).
func tuneTransport(t *http.Transport, concurrency, readBuffer int) {
	s3client.TuneTransport(t, concurrency)
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
		freshBuf   = flag.Bool("fresh-buffers", false, "allocate a new read buffer PER REQUEST instead of reusing one per worker, and retain them for the run (lith#350). This is the CAUSAL test for whether allocation pressure is what slows a lith mount's first bytes: a mount moves 1.21 GB through ~1200 fresh 1 MiB chunk buffers in a 153-fill burst, while this tool normally reuses one buffer per worker and allocates essentially nothing. If -fresh-buffers makes this tool slow at the same shape and depth, allocation is the cause, measured directly and with no lith code involved")
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

	// THE WIRE SPLIT, shared with the mount (lith#350). An external comparison needs both
	// programs reporting the SAME intervals through the SAME code, because the question is
	// which of them the endpoint answers slower -- and a difference in how the two measure
	// would be indistinguishable from a difference in what they measure.
	//
	// It is always on here. This is a diagnostic tool, so there is nothing to protect: the
	// mount keeps it behind --wire-ttfb because that flag forces the plain-*http.Client
	// path, and that is a real cost in production. Here the plain path is the only path.
	var wireMu sync.Mutex
	var wireSamples []s3client.WireSample
	collect := func(w s3client.WireSample) {
		wireMu.Lock()
		wireSamples = append(wireSamples, w)
		wireMu.Unlock()
	}
	// Built the way internal/s3client builds it on its TransportWrap path, so the transport
	// tuning is identical and only the RoundTripper wrapper differs.
	baseTransport := &http.Transport{}
	tuneTransport(baseTransport, poolSize, *readBuffer)
	httpClient := &http.Client{Transport: s3client.NewWireTracer(baseTransport, collect)}
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
		cursor      atomic.Int64
		retainedAll sync.Map
		bytesRead   atomic.Int64
		lat         = make([][]time.Duration, *workers)
		// FIRST-BYTE latency, kept separately from lat. lat is timed to after
		// io.ReadFull, so at an 8 MiB part the transfer buries the first byte entirely --
		// which is why this tool could not answer #350 until now. Recorded at the same
		// seam BlockStore.recordTTFB uses (immediately after the GET returns, before any
		// body read), so the two are the same quantity.
		ttfb = make([][]time.Duration, *workers)
		wg   sync.WaitGroup
	)
	deadline := time.Now().Add(*dur)
	rusStart := getCPU()
	wallStart := time.Now()

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, *partStr)
			// -fresh-buffers RETAINS every buffer for the run, which is the point: a
			// mount's chunk buffers go into the memory tier and stay reachable, so they
			// are promoted rather than collected cheaply. Allocating and dropping would
			// test a different and much kinder thing.
			var retained [][]byte
			var mine, mineTTFB []time.Duration
			for time.Now().Before(deadline) {
				idx := int(cursor.Add(1)-1) % len(reqs)
				r := reqs[idx]
				if *freshBuf {
					// Allocated BEFORE the request, as a fill does: the buffer exists
					// to receive the body, so the allocation is on the same side of
					// the timer as it is in lith.
					buf = make([]byte, *partStr)
					retained = append(retained, buf)
				}
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
				// GetObject returns once the response headers are in, so this is the
				// first-byte latency and not the transfer.
				mineTTFB = append(mineTTFB, time.Since(t0))
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
			ttfb[w] = mineTTFB
			// Keep the retained buffers reachable past the measurement so they cannot be
			// collected during it; this is what makes the arm comparable to a tier that
			// holds its chunks.
			if len(retained) > 0 {
				retainedAll.Store(w, retained)
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(wallStart)
	close(stopSample)
	rusEnd := getCPU()

	// Merge latencies. pctOf is shared so the full-request and first-byte percentiles
	// cannot drift apart in how they are computed.
	merge := func(per [][]time.Duration) []time.Duration {
		var all []time.Duration
		for _, m := range per {
			all = append(all, m...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		return all
	}
	pctOf := func(all []time.Duration, p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		i := int(p / 100 * float64(len(all)))
		if i >= len(all) {
			i = len(all) - 1
		}
		return all[i]
	}
	all := merge(lat)
	allTTFB := merge(ttfb)
	pct := func(p float64) time.Duration { return pctOf(all, p) }
	tpct := func(p float64) time.Duration { return pctOf(allTTFB, p) }
	// The fraction at or under a bound, which is how the evidence policy's input is scored
	// externally (#349): bucket{le=...}/count. Reported directly so a sweep does not need
	// a Prometheus scrape to answer "which latency regime is this".
	fracUnder := func(all []time.Duration, bound time.Duration) float64 {
		if len(all) == 0 {
			return 0
		}
		n := 0
		for _, d := range all {
			if d <= bound {
				n++
			}
		}
		return 100 * float64(n) / float64(len(all))
	}

	mbps := float64(bytesRead.Load()) / (1 << 20) / wall.Seconds()
	cpuSec := rusEnd - rusStart
	ncpu := float64(runtime.NumCPU())
	// The three intervals a first-byte latency is made of, split by connection reuse and
	// reported exactly as the mount reports them (#350).
	wireMu.Lock()
	ws := append([]s3client.WireSample(nil), wireSamples...)
	wireMu.Unlock()
	for _, reused := range []bool{false, true} {
		var wire, acq, wr, ep []time.Duration
		for _, w := range ws {
			if w.Reused != reused {
				continue
			}
			wire = append(wire, w.Wire)
			acq = append(acq, w.ConnAcquire)
			wr = append(wr, w.Write)
			ep = append(ep, w.Endpoint)
		}
		if len(wire) == 0 {
			continue
		}
		label := "new"
		if reused {
			label = "reused"
		}
		mean := func(d []time.Duration) float64 {
			var t time.Duration
			for _, x := range d {
				t += x
			}
			return float64(t.Microseconds()) / float64(len(d)) / 1000
		}
		// Means, not percentiles, because the question is which INTERVAL carries the
		// time and the three means must sum to the wire mean -- a property percentiles
		// do not have.
		fmt.Printf("SPLIT  conn=%-6s n=%4d  acquire=%7.2fms write=%7.3fms endpoint=%7.2fms  wire=%7.2fms\n",
			label, len(wire), mean(acq), mean(wr), mean(ep), mean(wire))
	}

	// TTFB line first: #350 is about whether depth buys throughput with LATENCY, so the
	// first-byte distribution and the aggregate belong side by side at every width.
	fmt.Printf("TTFB   workers=%d part=%dMiB fresh=%v auth=%s  n=%d  p10=%.1fms p50=%.1fms p90=%.1fms p99=%.1fms  <=25ms=%.1f%% <=50ms=%.1f%% <=60ms=%.1f%%\n",
		*workers, *partStr>>20, *freshBuf, authLabel(*noSign), len(allTTFB),
		float64(tpct(10).Microseconds())/1000, float64(tpct(50).Microseconds())/1000,
		float64(tpct(90).Microseconds())/1000, float64(tpct(99).Microseconds())/1000,
		fracUnder(allTTFB, 25*time.Millisecond), fracUnder(allTTFB, 50*time.Millisecond),
		fracUnder(allTTFB, 60*time.Millisecond))
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

// authLabel names the credential mode in the output line. Self-labelling because an
// external comparison ran for several cells with the mount SIGNING and this tool ANONYMOUS
// -- identical request fields, identical ranges, identical SDK, different credentials --
// and nothing in either program's output said so. Auth turned out to be worth only 6-21
// points rather than the gap under investigation, but it cost cells to find that out, and a
// confound that is visible in the output cannot persist silently.
func authLabel(noSign bool) string {
	if noSign {
		return "anon"
	}
	return "signed"
}
