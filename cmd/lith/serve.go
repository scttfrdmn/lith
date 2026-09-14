// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	lithnfs "github.com/scttfrdmn/lith/internal/nfs"
	"github.com/scttfrdmn/lith/internal/s3client"
)

type serveFlags struct {
	listen         string
	indexFile      string
	cargoship      string
	region         string
	memCache       string
	diskCache      string
	diskPath       string
	blockSize      string
	maxRange       string
	prefetchBudget string
	inflightBytes  string
	coalesceGap    string
	s3Concurrency  int
	prefetchConc   int
	nicGbps        float64
	autoIndexLimit int
	endpoint       string
	pathStyle      bool
	metrics        string
	clientIdle     time.Duration
	noPortmap      bool
	noSign         bool
	reqPays        bool
	logLevel       string
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve a lith mount to a cluster (read-only NFSv3 gateway)",
	}
	cmd.AddCommand(newServeNFSCmd())
	return cmd
}

func newServeNFSCmd() *cobra.Command {
	var f serveFlags
	cmd := &cobra.Command{
		Use:   "nfs s3://bucket[/prefix]",
		Short: "Export a lith mount over NFSv3 (read-only) for N compute nodes to share",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, prefix, err := parseS3URL(args[0])
			if err != nil {
				return err
			}
			return runServeNFS(cmd.Context(), &f, bucket, prefix)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.listen, "listen", ":2049", "listen address for NFS + embedded MOUNT")
	fl.StringVar(&f.indexFile, "index-file", "", "index file to load (built by listing if absent)")
	fl.StringVar(&f.cargoship, "cargoship", "", "export a CargoShip 2.1 archive: build the index in-process from this manifest s3:// URL (fail-closed, never lists)")
	fl.StringVar(&f.region, "region", "", "bucket region (resolved if empty)")
	fl.StringVar(&f.memCache, "mem-cache", "", "memory block cache size (default: 25% of system memory)")
	fl.StringVar(&f.diskCache, "disk-cache", "0", "disk block cache size (0 disables); on a gateway, size it to the working set so a warm re-read and a restart serve from local disk, not S3")
	fl.StringVar(&f.diskPath, "disk-path", "", "disk cache directory (default $TMPDIR/lith-cache)")
	// Block-store / S3-client tuning — parity with `lith mount` for the flags that
	// configure the read path, cache, budget, and S3 client the gateway uses.
	fl.StringVar(&f.blockSize, "block-size", "8MiB", "block size (1MiB-64MiB)")
	fl.StringVar(&f.maxRange, "max-range", "64MiB", "max coalesced range GET size")
	fl.StringVar(&f.prefetchBudget, "prefetch-budget", "", "max bytes of un-demanded prefetch (default: 50% of --mem-cache)")
	fl.StringVar(&f.inflightBytes, "inflight-bytes", "", "max bytes in flight to S3 (default: 2 × NIC bandwidth × 100ms)")
	fl.StringVar(&f.coalesceGap, "coalesce-gap", "0", "largest gap between fill ranges merged into one GET; 0 = derive from NIC × TTFB (#124)")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 128, "max concurrent S3 requests")
	fl.IntVar(&f.prefetchConc, "prefetch-concurrency", 0, "max concurrent prefetch fills (0 = --s3-concurrency)")
	fl.Float64Var(&f.nicGbps, "nic-gbps", 0, "NIC bandwidth in Gbps for the coalesce gap / inflight budget (0 = a fixed fallback; serve does not autodetect)")
	fl.IntVar(&f.autoIndexLimit, "auto-index-limit", 5_000_000, "max keys to auto-index when no --index-file/--cargoship is given")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style S3 addressing")
	fl.StringVar(&f.metrics, "metrics", "", "serve Prometheus metrics on this address (e.g. :9101)")
	fl.StringVar(&f.logLevel, "log-level", "info", "log verbosity: debug, info, warn, error")
	fl.DurationVar(&f.clientIdle, "client-idle", 5*time.Minute, "release a client's readahead share after this idle time")
	fl.BoolVar(&f.noPortmap, "no-portmap", true, "do not register with rpcbind; clients mount with an explicit port (mountport=)")
	fl.BoolVar(&f.noSign, "no-sign-request", false, "anonymous S3 requests (public buckets)")
	fl.BoolVar(&f.reqPays, "requester-pays", false, "add the requester-pays header")
	return cmd
}

func runServeNFS(ctx context.Context, f *serveFlags, bucket, prefix string) error {
	log := newLoggerAt(parseLogLevel(f.logLevel))
	slog.SetDefault(log) // so package-level slog.Debug diagnostics honor --log-level
	if f.cargoship != "" && f.indexFile != "" {
		return fmt.Errorf("--cargoship and --index-file are mutually exclusive")
	}
	memCache := defaultMemCacheBytes()
	if f.memCache != "" {
		var err error
		if memCache, err = parseSize(f.memCache); err != nil {
			return err
		}
	}

	client, err := newS3Client(ctx, s3client.Config{
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSign, RequesterPays: f.reqPays,
		Endpoint: f.endpoint, PathStyle: f.pathStyle, Concurrency: f.s3Concurrency,
	})
	if err != nil {
		return err
	}

	var met *metrics.Metrics
	if f.metrics != "" {
		met = metrics.New()
	}

	// Load the index and derive the handle root id (first 8 bytes of a stable
	// identity: the index file's sha256, else the CargoShip manifest sha, else
	// the bucket+prefix). A restart against the same index reproduces it; a
	// rebuilt index yields a different one, so old handles go STALE (#144).
	// Pointer-based gateway (#167): s3://bucket/dataset@current or @<version-id>.
	// Resolve the published version's prebuilt index and serve the archive's own
	// file tree (root ""). The handle root id is derived from the version, so a
	// version change would change every handle — which is exactly why the gateway
	// refuses a live version swap (see the SIGHUP handler below).
	dataset, ref := splitPointerRef(prefix)
	rootPrefix := prefix
	var (
		ix            *index.Index
		closeIdx      func() error
		rootID        [8]byte
		servedVersion string
	)
	if ref != "" {
		if f.cargoship != "" || f.indexFile != "" {
			return fmt.Errorf("s3://…@%s is mutually exclusive with --cargoship and --index-file", ref)
		}
		ix, servedVersion, err = resolvePointer(ctx, client, bucket, dataset, ref)
		if err != nil {
			return err // fail closed: no fallback to listing (#141)
		}
		rootPrefix = ""
		rootID = pointerRootID(bucket, dataset, servedVersion)
		log.Info("serving published dataset", "dataset", dataset, "ref", ref, "version", servedVersion)
	} else {
		ix, closeIdx, rootID, err = loadServeIndex(ctx, f, client, bucket, prefix, log)
		if err != nil {
			return err
		}
	}
	if closeIdx != nil {
		defer func() { _ = closeIdx() }()
	}
	reader, err := ix.Root(rootPrefix)
	if err != nil {
		return err
	}

	diskCache, err := parseSize(f.diskCache)
	if err != nil {
		return err
	}
	diskPath := f.diskPath
	if diskPath == "" {
		diskPath = filepath.Join(os.TempDir(), "lith-cache")
	}
	blockSize, err := parseSize(f.blockSize)
	if err != nil {
		return err
	}
	if blockSize < 1<<20 || blockSize > 64<<20 {
		return fmt.Errorf("block size must be between 1MiB and 64MiB")
	}
	maxRange, err := parseSize(f.maxRange)
	if err != nil {
		return err
	}
	coalesceGap, err := parseSize(f.coalesceGap)
	if err != nil {
		return err
	}
	var prefetchBudget, inflight int64
	if f.prefetchBudget != "" {
		if prefetchBudget, err = parseSize(f.prefetchBudget); err != nil {
			return err
		}
	}
	if f.inflightBytes != "" {
		if inflight, err = parseSize(f.inflightBytes); err != nil {
			return err
		}
	}
	var nicBytesPerSec int64
	if f.nicGbps > 0 {
		nicBytesPerSec = int64(f.nicGbps * 1e9 / 8)
	}
	bs, err := blockstore.New(client, blockstore.Config{
		Bucket: bucket, BlockSize: blockSize, MemCache: memCache, MaxRange: maxRange,
		DiskCache: diskCache, DiskPath: diskPath, DiskWriters: 8,
		S3Concurrency: f.s3Concurrency, PrefetchConcurrency: f.prefetchConc,
		PrefetchBudget: prefetchBudget, InflightBytes: inflight,
		CoalesceGap: coalesceGap, NICBytesPerSec: nicBytesPerSec,
		Recorder: met,
	})
	if err != nil {
		return err
	}
	defer bs.Close()

	if f.metrics != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", met.Handler())
		msrv := &http.Server{Addr: f.metrics, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := msrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("metrics server", "err", err)
			}
		}()
		defer func() { _ = msrv.Close() }()
	}

	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", f.listen, err)
	}
	log.Info("lith NFSv3 gateway", "listen", f.listen, "bucket", bucket, "prefix", prefix,
		"keys", reader.Len(), "root_id", fmt.Sprintf("%x", rootID), "portmap", !f.noPortmap)
	fmt.Fprintf(os.Stderr, "mount with: sudo mount -t nfs -o vers=3,proto=tcp,port=%s,mountport=%s,nolock <host>:/ /mnt\n", portOf(f.listen), portOf(f.listen))

	// Gateway refresh (#167): unlike a single-client FUSE mount, the gateway does
	// NOT hot-swap. A version change would change every handle's RootID and STALE
	// all connected clients, and NFSv3 is stateless so those clients cannot be
	// detected. So on SIGHUP the gateway re-resolves and REFUSES a version change
	// with a clear, actionable message (this is a documented steady state, not an
	// operator error). A pointer that still resolves to the served version is a
	// legitimate no-op success. The handler is installed for @ref gateways so a
	// refresh attempt logs the refusal rather than SIGHUP's default (terminate).
	if ref != "" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				_, resolved, rerr := resolvePointer(ctx, client, bucket, dataset, ref)
				if rerr != nil {
					log.Warn("refresh: cannot re-resolve the pointer; gateway unchanged", "serving", servedVersion, "err", rerr)
					continue
				}
				if !gatewayRefreshRefused(servedVersion, resolved) {
					log.Info("refresh: gateway already serving the current version (no-op)", "version", servedVersion)
					continue
				}
				log.Warn("refresh REFUSED on the NFS gateway: adopting a new version would change every file handle and STALE all connected clients, and NFSv3 is stateless so connected clients cannot be detected. The gateway keeps serving the current version. To adopt the new version: restart the gateway, then remount clients.",
					"serving", servedVersion, "available", resolved)
			}
		}()
	}

	return lithnfs.Serve(ctx, ln, lithnfs.Config{
		Index: reader, Store: bs, RootID: rootID, ClientIdle: f.clientIdle, Metrics: met, Logger: log,
	})
}

// gatewayRefreshRefused reports whether a gateway refresh must be refused: the
// pointer resolved to a version DIFFERENT from the one being served (adopting it
// would STALE every client handle). Resolving to the same version is a legitimate
// no-op success, not a refusal.
func gatewayRefreshRefused(served, resolved string) bool {
	return resolved != "" && resolved != served
}

// pointerRootID derives an NFS handle root id from a published version: stable
// while a gateway serves that version (so a restart reproduces handles), and
// different across versions (so adopting a new version is a clean STALE, never a
// silent reinterpretation of an existing handle).
func pointerRootID(bucket, dataset, versionID string) [8]byte {
	sum := sha256.Sum256([]byte("published:" + bucket + "/" + dataset + "@" + versionID))
	var id [8]byte
	copy(id[:], sum[:8])
	return id
}

// loadServeIndex loads/builds the index and returns it, a close func, and the
// derived 8-byte handle root id.
func loadServeIndex(ctx context.Context, f *serveFlags, client s3client.API, bucket, prefix string, log *slog.Logger) (*index.Index, func() error, [8]byte, error) {
	var rootID [8]byte
	switch {
	case f.indexFile != "":
		if _, err := os.Stat(f.indexFile); err != nil {
			return nil, nil, rootID, fmt.Errorf("index file %s: %w", f.indexFile, err)
		}
		raw, err := os.ReadFile(f.indexFile)
		if err != nil {
			return nil, nil, rootID, err
		}
		sum := sha256.Sum256(raw)
		copy(rootID[:], sum[:8])
		ix, closeFn, oerr := index.Open(f.indexFile)
		if oerr != nil {
			return nil, nil, rootID, fmt.Errorf("open index %s: %w", f.indexFile, oerr)
		}
		return ix, closeFn, rootID, nil
	case f.cargoship != "":
		manBucket, manKey, perr := parseCargoshipManifestArg(f.cargoship, bucket)
		if perr != nil {
			return nil, nil, rootID, perr
		}
		if manBucket != bucket {
			return nil, nil, rootID, fmt.Errorf("--cargoship manifest bucket %q != mount bucket %q", manBucket, bucket)
		}
		ix, err := buildCargoshipIndex(ctx, client, bucket, manKey, false, log)
		if err != nil {
			return nil, nil, rootID, err
		}
		sum := sha256.Sum256([]byte("cargoship:" + bucket + "/" + manKey))
		copy(rootID[:], sum[:8])
		return ix, nil, rootID, nil
	default:
		ix, err := index.BuildFromList(ctx, client, index.ListOptions{
			Options: index.Options{Bucket: bucket, Prefix: prefix, Logger: log},
			MaxKeys: f.autoIndexLimit,
		})
		if err != nil {
			return nil, nil, rootID, err
		}
		sum := sha256.Sum256([]byte("list:" + bucket + "/" + prefix))
		copy(rootID[:], sum[:8])
		return ix, nil, rootID, nil
	}
}

func portOf(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil && p != "" {
		return p
	}
	return "2049"
}
