// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	fusefs "github.com/scttfrdmn/lith/internal/fuse"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
)

type mountFlags struct {
	indexFile        string
	memCache         string
	diskCache        string
	diskPath         string
	blockSize        string
	maxRange         string
	smallFile        string
	partsMax         string
	coalesceGap      string
	bgzfWholeFileMax string
	footerTier2      bool
	s3Concurrency    int
	prefetchConc     int
	prefetchBudget   string
	maxReadahead     int64
	nicGbps          float64
	siblingWindow    int
	siblingRead      int
	diskWriters      int
	inflightBytes    string
	metrics          string
	pprof            string
	timelineCSV      string
	allowOther       bool
	uid              int
	gid              int
	exec             bool
	autoIndexLimit   int
	daemon           bool

	// S3 client options (shared with index build).
	noSignRequest bool
	requesterPays bool
	endpoint      string
	pathStyle     bool
	region        string
}

func newMountCmd() *cobra.Command {
	var f mountFlags
	cmd := &cobra.Command{
		Use:   "mount s3://bucket[/prefix] /mnt/point",
		Short: "Mount an S3 bucket as a read-only POSIX filesystem",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, prefix, err := parseS3URL(args[0])
			if err != nil {
				return err
			}
			return runMount(cmd.Context(), &f, bucket, prefix, args[1])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.indexFile, "index-file", "", "index file to load (built automatically if absent and under --auto-index-limit). A whole-bucket or parent-prefix index can back mounts rooted at any prefix at or below its own root, concurrently")
	fl.StringVar(&f.memCache, "mem-cache", "", "memory block cache size (default: 25% of system memory)")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk block cache size (0 disables)")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory (default $TMPDIR/lith-cache)")
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "block size (1MiB-64MiB)")
	fl.StringVar(&f.maxRange, "max-range", "64MiB", "max coalesced range GET size")
	fl.StringVar(&f.smallFile, "small-file", "4MiB", "fetch files at or below this size whole on first read")
	fl.StringVar(&f.partsMax, "parts-max", "64MiB", "fetch files at or below this size whole as concurrent block-sized range parts on first read (0 disables; a single GET below one block)")
	fl.StringVar(&f.coalesceGap, "coalesce-gap", "0", "largest gap between two format-plan/demand fill ranges merged into one range GET (#124); 0 = derive from the NIC baseline × measured first-byte latency, clamped to [256KiB, 64MiB]")
	fl.StringVar(&f.bgzfWholeFileMax, "bgzf-whole-file-max", "512MiB", "for a bgzf data file (BAM/CRAM/VCF.gz) with an index sibling, prefetch it whole on open when at or below this size; above it, prefetch only the index-resolved slice ranges (#107)")
	fl.BoolVar(&f.footerTier2, "footer-tier2", true, "prefetch the index-resolved projection (Parquet column chunks) / entries (zip) for footer-family files (#108); false leaves only the generic tier-1 footer+head prefetch")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 128, "max concurrent S3 requests")
	fl.IntVar(&f.prefetchConc, "prefetch-concurrency", 0, "max concurrent prefetch fills (0 = --s3-concurrency)")
	fl.StringVar(&f.prefetchBudget, "prefetch-budget", "", "max bytes of un-demanded prefetch (default: 50% of --mem-cache)")
	fl.Int64Var(&f.maxReadahead, "max-readahead", 0, "max sequential readahead window in blocks (0 = 1.5x the bandwidth-delay product, inflight-bytes/block; the 1.5x is empirical, measured on c8gd.16xlarge)")
	fl.Float64Var(&f.nicGbps, "nic-gbps", 0, "override the detected NIC bandwidth in Gbps (sizes --inflight-bytes and the readahead window); 0 = detect via ethtool, then EC2 DescribeInstanceTypes baseline, then a fixed fallback")
	fl.IntVar(&f.siblingWindow, "sibling-window", 4, "max index-position gap between successive opens in a directory that still counts as walking it in key order (#63)")
	fl.IntVar(&f.siblingRead, "sibling-readahead", 16, "how many following siblings a detected directory walk prefetches whole (0 disables)")
	fl.IntVar(&f.diskWriters, "disk-writers", 4, "write-behind workers for the disk cache")
	fl.StringVar(&f.inflightBytes, "inflight-bytes", "", "max bytes in flight to S3 (default: 2 × NIC bandwidth × 100ms)")
	fl.StringVar(&f.metrics, "metrics", "", "serve Prometheus metrics on this address (e.g. :9101); serves only /metrics (no pprof)")
	fl.StringVar(&f.pprof, "pprof", "", "serve net/http/pprof debug handlers on this address (e.g. 127.0.0.1:6060); off by default. SECURITY: exposes argv and an on-demand CPU/goroutine profiling DoS — bind to localhost and never expose to untrusted networks")
	fl.StringVar(&f.timelineCSV, "timeline-csv", "", "diagnostic (#70): record a per-chunk demand-read timeline (join-wait, in-flight depth, prefetch-dispatch→open lag) and write it here on unmount")
	fl.BoolVar(&f.allowOther, "allow-other", false, "allow other users to access the mount")
	fl.IntVar(&f.uid, "uid", os.Getuid(), "owner uid for all files")
	fl.IntVar(&f.gid, "gid", os.Getgid(), "owner gid for all files")
	fl.BoolVar(&f.exec, "exec", false, "report files as mode 0555 instead of 0444")
	fl.IntVar(&f.autoIndexLimit, "auto-index-limit", 5_000_000, "max keys to auto-index at mount")
	fl.BoolVar(&f.daemon, "daemon", false, "fork into the background after the mount is ready")
	fl.BoolVar(&f.noSignRequest, "no-sign-request", false, "send anonymous requests (public buckets)")
	fl.BoolVar(&f.requesterPays, "requester-pays", false, "add the requester-pays header to every request")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.region, "region", "", "bucket region (resolved from the bucket if empty)")
	return cmd
}

func runMount(ctx context.Context, f *mountFlags, bucket, prefix, mountpoint string) error {
	if f.daemon && !isDaemonChild() {
		return daemonize()
	}
	log := newLogger()

	blockSize, err := parseSize(f.blockSize)
	if err != nil {
		return err
	}
	if blockSize < 1<<20 || blockSize > 64<<20 {
		return fmt.Errorf("block size must be between 1MiB and 64MiB")
	}
	memCache := defaultMemCacheBytes()
	if f.memCache != "" {
		memCache, err = parseSize(f.memCache)
		if err != nil {
			return err
		}
	}
	diskCache, err := parseSize(f.diskCache)
	if err != nil {
		return err
	}
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
	smallFile, err := parseSize(f.smallFile)
	if err != nil {
		return err
	}
	coalesceGap, err := parseSize(f.coalesceGap)
	if err != nil {
		return err
	}
	partsMax, err := parseSize(f.partsMax)
	if err != nil {
		return err
	}
	bgzfWholeFileMax, err := parseSize(f.bgzfWholeFileMax)
	if err != nil {
		return err
	}
	diskPath := f.diskPath
	if diskPath == "" {
		diskPath = filepath.Join(os.TempDir(), "lith-cache")
	}
	if diskCache > 0 {
		if w := diskCacheWarning(diskPath); w != "" {
			log.Warn("disk cache location", "path", diskPath, "warning", w)
		}
	}

	client, err := newS3Client(ctx, s3client.Config{
		Bucket:        bucket,
		Region:        f.region,
		NoSignRequest: f.noSignRequest,
		RequesterPays: f.requesterPays,
		Endpoint:      f.endpoint,
		PathStyle:     f.pathStyle,
		Concurrency:   f.s3Concurrency,
	})
	if err != nil {
		return err
	}

	ix, closeIdx, err := loadOrBuildIndex(ctx, f, client, bucket, prefix, log)
	if err != nil {
		return err
	}
	if closeIdx != nil {
		defer func() { _ = closeIdx() }()
	}

	// Root the mount at the requested prefix. For an auto-built index this is a
	// pass-through (it was built at the prefix); for a loaded index built at or
	// above the prefix, it is a sub-root view — so one whole-bucket index can
	// back many prefix mounts (#90). Rejects a prefix the index cannot serve.
	root, err := ix.Root(prefix)
	if err != nil {
		return err
	}

	var met *metrics.Metrics
	if f.metrics != "" {
		met = metrics.New()
	}
	// Diagnostic (#70): when --timeline-csv is set, the per-chunk timeline
	// recorder is the block-store Recorder (it also tallies the basic counters).
	// It implements the optional chunkTimelineRecorder extension, so the block
	// store captures join-wait/dispatch events; a plain metrics recorder does not.
	var tl *mountTimeline
	var rec blockstore.Recorder = met // nil-safe
	if f.timelineCSV != "" {
		tl = newMountTimeline()
		rec = tl
	}
	// Resolve NIC bandwidth (baseline drives the in-flight budget; #79). Cache
	// nic.json next to the index so repeat mounts and boxes without
	// ec2:DescribeInstanceTypes still get a real answer.
	nicDir := os.TempDir()
	if f.indexFile != "" {
		nicDir = filepath.Dir(f.indexFile)
	}
	nic := resolveNIC(ctx, nicDir, f.nicGbps)
	if nic.Source != "" {
		log.Info("nic bandwidth", "baseline_gbps", nic.BaselineGbps, "peak_gbps", nic.PeakGbps, "source", nic.Source)
	} else {
		log.Info("nic bandwidth", "source", "unknown (using fixed in-flight fallback)")
	}
	inflight, inflightDesc := computeInflightBytes(f.inflightBytes, nic.BaselineGbps)
	log.Info("inflight-bytes budget", "budget", inflightDesc)
	// Device-derived coalesce gap inputs (#124/session 30): NIC baseline in
	// bytes/s and a first-byte-latency seed (refined by the blockstore's rolling
	// median of measured fills). 40 ms is a typical in-region S3 first byte.
	nicBytesPerSec := int64(nic.BaselineGbps * 1e9 / 8)
	ttfbSeed := 40 * time.Millisecond
	// Default the readahead window to the bandwidth-delay product so a single
	// reader can fill the NIC on a cold read (#56).
	f.maxReadahead = effectiveReadahead(f.maxReadahead, inflight, blockSize)
	log.Info("readahead window", "blocks", f.maxReadahead)
	bs, err := blockstore.New(client, blockstore.Config{
		Bucket:              bucket,
		BlockSize:           blockSize,
		MemCache:            memCache,
		DiskCache:           diskCache,
		DiskPath:            diskPath,
		MaxRange:            maxRange,
		S3Concurrency:       f.s3Concurrency,
		PrefetchConcurrency: f.prefetchConc,
		PrefetchBudget:      prefetchBudget,
		CoalesceGap:         coalesceGap, // 0 → device-derived
		NICBytesPerSec:      nicBytesPerSec,
		TTFB:                ttfbSeed,
		DiskWriters:         f.diskWriters,
		InflightBytes:       inflight,
		Recorder:            rec, // nil-safe; timeline recorder when --timeline-csv
	})
	if err != nil {
		return err
	}
	defer bs.Close()
	effConc := f.prefetchConc
	if effConc <= 0 {
		effConc = f.s3Concurrency
	}
	if coalesceGap > 0 {
		log.Info("coalesce gap", "bytes", bs.CoalesceGap(), "source", "--coalesce-gap")
	} else {
		// The gap is NIC_baseline × TTFB / C, C = usable prefetch concurrency (#31);
		// the value logged is at full concurrency (FillBatch caps C at a batch's run
		// count). If it is ≥ a fill block, footer projections stream instead.
		log.Info("coalesce gap", "bytes", bs.CoalesceGap(), "source", "device-derived",
			"nic_bytes_per_s", nicBytesPerSec, "ttfb_seed", ttfbSeed, "concurrency", effConc)
	}
	met.RegisterQueueDepth(func() float64 { return float64(bs.QueueDepth()) })

	// The single prefetch policy object (#64): per-handle readahead, sibling
	// readahead (#63), and small-file parts (#69) all draw on the one budget and
	// query the Index for neighborhoods through it.
	limits := prefetch.NewPolicy(
		bs.PrefetchBudgetBytes(),
		prefetch.DeviceLimits{
			NICBDPBytes:    inflight,
			MemCacheBytes:  memCache,
			DiskWriteBPS:   0, // populated once the disk tier reports a sustained rate
			NICBytesPerSec: nicBytesPerSec,
			TTFB:           ttfbSeed,
		},
		func(key string, n int) []prefetch.Sibling {
			sibs := root.Neighborhood(key, n)
			out := make([]prefetch.Sibling, len(sibs))
			for i, s := range sibs {
				out[i] = prefetch.Sibling{Key: s.Key, Size: s.Size, ETagHash: s.ETagHash}
			}
			return out
		},
	)

	fcfg := fusefs.Config{
		Index:              root,
		Store:              bs,
		Metrics:            met,
		UID:                uint32(f.uid),
		GID:                uint32(f.gid),
		SmallFile:          smallFile,
		PartsMax:           partsMax,
		BgzfWholeFileMax:   bgzfWholeFileMax,
		DisableFooterTier2: !f.footerTier2,
		MaxReadahead:       f.maxReadahead,
		SiblingWindow:      f.siblingWindow,
		SiblingReadahead:   f.siblingRead,
		Limits:             limits,
	}

	srv, err := fusefs.Mount(mountpoint, fcfg, fusefs.MountOptions{
		AllowOther: f.allowOther,
		FsName:     "s3://" + bucket,
	})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	log.Info("mounted", "bucket", bucket, "prefix", prefix, "mountpoint", mountpoint, "root", root.Prefix(), "keys", root.Len())
	log.Info("s3 transport", "info", s3client.TransportInfo(f.s3Concurrency), "s3_concurrency", f.s3Concurrency)

	// Record the mount so `lith mounts`/`lith umount` can find it (#91); removed
	// on clean exit. Best-effort — a record failure must not fail the mount.
	if p, werr := writeMountRecord(mountRecord{
		PID: os.Getpid(), Mountpoint: mountpoint, Bucket: bucket, Root: root.Prefix(),
		IndexFile: f.indexFile, Start: time.Now(),
	}); werr != nil {
		log.Warn("could not write mount record", "err", werr)
	} else {
		defer removeMountRecord(mountpoint)
		_ = p
	}

	// Serve Prometheus metrics if requested. This endpoint serves ONLY /metrics;
	// pprof lives behind the separate opt-in --pprof flag below so scrape targets
	// (often reachable network-wide) never expose the pprof surface.
	var metricsSrv *http.Server
	if met != nil {
		metricsSrv = &http.Server{Addr: f.metrics, Handler: newMetricsMux(met)}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("metrics server stopped", "err", err)
			}
		}()
		log.Info("metrics endpoint", "addr", f.metrics)
	}

	// Serve net/http/pprof only when explicitly opted in via --pprof. The mutex
	// and block profilers (which tax the hot path) are enabled only here, so a
	// plain --metrics mount pays none of that cost. SECURITY: this surface leaks
	// argv (/cmdline) and allows an on-demand CPU/goroutine DoS (/profile, /trace)
	// — the flag help recommends binding to localhost.
	var pprofSrv *http.Server
	if f.pprof != "" {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		pprofSrv = &http.Server{Addr: f.pprof, Handler: newPprofMux()}
		go func() {
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("pprof server stopped", "err", err)
			}
		}()
		log.Info("pprof endpoint", "addr", f.pprof)
	}

	// If we were forked as a daemon child, tell the parent the mount is ready.
	signalDaemonReady()

	// Clean unmount on SIGINT/SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("unmounting")
		if metricsSrv != nil {
			_ = metricsSrv.Close()
		}
		if pprofSrv != nil {
			_ = pprofSrv.Close()
		}
		if err := srv.Unmount(); err != nil {
			log.Warn("unmount failed; retrying via lazy unmount", "err", err)
		}
	}()

	srv.Wait() // blocks until unmounted
	log.Info("unmounted", "stale_objects", len(bs.StaleKeys()))

	// Flush the #70 timeline on clean unmount.
	if tl != nil {
		if n, werr := tl.writeCSV(f.timelineCSV); werr != nil {
			log.Warn("timeline CSV write failed", "err", werr)
		} else {
			log.Info("timeline written", "file", f.timelineCSV, "rows", n)
			_, _ = fmt.Fprint(os.Stderr, "\n=== #70 chunk timeline ===\n", tl.summary())
		}
	}
	return nil
}

// newMetricsMux builds the --metrics mux. It serves ONLY /metrics; the pprof
// handlers are deliberately not registered here (see F1) so a network-reachable
// scrape target does not expose the pprof surface.
func newMetricsMux(met *metrics.Metrics) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", met.Handler())
	return mux
}

// newPprofMux builds the --pprof mux with the net/http/pprof handlers. It is
// wired only when --pprof is set; the addr should be a localhost address.
func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// loadOrBuildIndex loads the index from --index-file, or builds it from the
// bucket when the file is absent (capped by --auto-index-limit). The returned
// close function (if non-nil) releases an mmap.
func loadOrBuildIndex(ctx context.Context, f *mountFlags, client s3client.API, bucket, prefix string, log *slog.Logger) (*index.Index, func() error, error) {
	if f.indexFile != "" {
		if _, err := os.Stat(f.indexFile); err == nil {
			ix, closeFn, oerr := index.Open(f.indexFile)
			if oerr != nil {
				return nil, nil, fmt.Errorf("open index %s: %w", f.indexFile, oerr)
			}
			log.Info("loaded index", "file", f.indexFile, "keys", ix.Len())
			return ix, closeFn, nil
		}
	}

	// Auto-build (bounded by the auto-index limit).
	log.Info("building index", "bucket", bucket, "prefix", prefix, "auto_index_limit", f.autoIndexLimit)
	ix, err := index.BuildFromList(ctx, client, index.ListOptions{
		Options: index.Options{Bucket: bucket, Prefix: prefix, Exec: f.exec, Logger: log},
		MaxKeys: f.autoIndexLimit,
	})
	if err != nil {
		if errors.Is(err, index.ErrTooManyKeys) {
			return nil, nil, fmt.Errorf("bucket has more than %d keys; run `lith index build s3://%s/%s --index-file <file>` first, then mount with --index-file",
				f.autoIndexLimit, bucket, prefix)
		}
		return nil, nil, err
	}
	if f.indexFile != "" {
		if err := ix.Save(f.indexFile); err != nil {
			log.Warn("failed to persist auto-built index", "err", err)
		}
	}
	return ix, nil, nil
}
