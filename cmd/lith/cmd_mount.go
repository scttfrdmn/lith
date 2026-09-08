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

	"github.com/scttfrdmn/lith/internal/blockstore"
	fusefs "github.com/scttfrdmn/lith/internal/fuse"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
)

type mountFlags struct {
	indexFile      string
	memCache       string
	diskCache      string
	diskPath       string
	blockSize      string
	maxRange       string
	smallFile      string
	s3Concurrency  int
	prefetchConc   int
	prefetchBudget string
	maxReadahead   int64
	diskWriters    int
	inflightBytes  string
	metrics        string
	allowOther     bool
	uid            int
	gid            int
	exec           bool
	autoIndexLimit int
	daemon         bool

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
	fl.StringVar(&f.indexFile, "index-file", "", "index file to load (built automatically if absent and under --auto-index-limit)")
	fl.StringVar(&f.memCache, "mem-cache", "", "memory block cache size (default: 25% of system memory)")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk block cache size (0 disables)")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory (default $TMPDIR/lith-cache)")
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "block size (1MiB-64MiB)")
	fl.StringVar(&f.maxRange, "max-range", "64MiB", "max coalesced range GET size")
	fl.StringVar(&f.smallFile, "small-file", "4MiB", "fetch files at or below this size whole on first read")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 128, "max concurrent S3 requests")
	fl.IntVar(&f.prefetchConc, "prefetch-concurrency", 0, "max concurrent prefetch fills (0 = --s3-concurrency)")
	fl.StringVar(&f.prefetchBudget, "prefetch-budget", "", "max bytes of un-demanded prefetch (default: 50% of --mem-cache)")
	fl.Int64Var(&f.maxReadahead, "max-readahead", 64, "max sequential readahead window in blocks")
	fl.IntVar(&f.diskWriters, "disk-writers", 4, "write-behind workers for the disk cache")
	fl.StringVar(&f.inflightBytes, "inflight-bytes", "", "max bytes in flight to S3 (default: 2 × NIC bandwidth × 100ms)")
	fl.StringVar(&f.metrics, "metrics", "", "serve Prometheus metrics and pprof on this address (e.g. :9101)")
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

	var met *metrics.Metrics
	if f.metrics != "" {
		met = metrics.New()
	}
	inflight, inflightDesc := computeInflightBytes(f.inflightBytes)
	log.Info("inflight-bytes budget", "budget", inflightDesc)
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
		DiskWriters:         f.diskWriters,
		InflightBytes:       inflight,
		Recorder:            met, // nil-safe
	})
	if err != nil {
		return err
	}
	defer bs.Close()
	met.RegisterQueueDepth(func() float64 { return float64(bs.QueueDepth()) })

	fcfg := fusefs.Config{
		Index:        ix,
		Store:        bs,
		Metrics:      met,
		UID:          uint32(f.uid),
		GID:          uint32(f.gid),
		SmallFile:    smallFile,
		MaxReadahead: f.maxReadahead,
	}

	srv, err := fusefs.Mount(mountpoint, fcfg, fusefs.MountOptions{
		AllowOther: f.allowOther,
		FsName:     "s3://" + bucket,
	})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	log.Info("mounted", "bucket", bucket, "prefix", prefix, "mountpoint", mountpoint, "keys", ix.Len())
	log.Info("s3 transport", "info", s3client.TransportInfo(f.s3Concurrency), "s3_concurrency", f.s3Concurrency)

	// Serve metrics (and pprof) if requested.
	var metricsSrv *http.Server
	if met != nil {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		mux := http.NewServeMux()
		mux.Handle("/metrics", met.Handler())
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		metricsSrv = &http.Server{Addr: f.metrics, Handler: mux}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("metrics server stopped", "err", err)
			}
		}()
		log.Info("metrics endpoint", "addr", f.metrics)
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
		if err := srv.Unmount(); err != nil {
			log.Warn("unmount failed; retrying via lazy unmount", "err", err)
		}
	}()

	srv.Wait() // blocks until unmounted
	log.Info("unmounted", "stale_objects", len(bs.StaleKeys()))
	return nil
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
