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
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	lithnfs "github.com/scttfrdmn/lith/internal/nfs"
	"github.com/scttfrdmn/lith/internal/s3client"
)

type serveFlags struct {
	listen     string
	indexFile  string
	cargoship  string
	region     string
	memCache   string
	diskCache  string
	diskPath   string
	metrics    string
	clientIdle time.Duration
	noPortmap  bool
	noSign     bool
	reqPays    bool
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
	fl.StringVar(&f.metrics, "metrics", "", "serve Prometheus metrics on this address (e.g. :9101)")
	fl.DurationVar(&f.clientIdle, "client-idle", 5*time.Minute, "release a client's readahead share after this idle time")
	fl.BoolVar(&f.noPortmap, "no-portmap", true, "do not register with rpcbind; clients mount with an explicit port (mountport=)")
	fl.BoolVar(&f.noSign, "no-sign-request", false, "anonymous S3 requests (public buckets)")
	fl.BoolVar(&f.reqPays, "requester-pays", false, "add the requester-pays header")
	return cmd
}

func runServeNFS(ctx context.Context, f *serveFlags, bucket, prefix string) error {
	log := newLogger()
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
		Bucket: bucket, Region: f.region, NoSignRequest: f.noSign, RequesterPays: f.reqPays, Concurrency: 128,
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
	ix, closeIdx, rootID, err := loadServeIndex(ctx, f, client, bucket, prefix, log)
	if err != nil {
		return err
	}
	if closeIdx != nil {
		defer func() { _ = closeIdx() }()
	}
	reader, err := ix.Root(prefix)
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
	bs, err := blockstore.New(client, blockstore.Config{
		Bucket: bucket, BlockSize: 8 << 20, MemCache: memCache, MaxRange: 64 << 20,
		DiskCache: diskCache, DiskPath: diskPath, DiskWriters: 8,
		S3Concurrency: 128, Recorder: met,
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

	return lithnfs.Serve(ctx, ln, lithnfs.Config{
		Index: reader, Store: bs, RootID: rootID, ClientIdle: f.clientIdle, Metrics: met, Logger: log,
	})
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
