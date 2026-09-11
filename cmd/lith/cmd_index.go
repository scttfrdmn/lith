// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client"
	"github.com/spf13/cobra"
)

// newS3Client constructs the S3 client for the build/refresh commands. It is a
// package variable so tests can substitute an in-process fake.
var newS3Client = func(ctx context.Context, cfg s3client.Config) (s3client.API, error) {
	return s3client.New(ctx, cfg)
}

// indexFlags holds the flags shared by build and refresh.
type indexFlags struct {
	indexFile        string
	noSignRequest    bool
	requesterPays    bool
	endpoint         string
	pathStyle        bool
	region           string
	shards           []string
	exec             bool
	pageSize         int32
	inventory        string // s3://dest-bucket/path/manifest.json, or a local manifest path
	keys             string // --keys: local path to an explicit key list
	keysFromManifest string // --keys-from-manifest: s3:// URL or local path (gz ok)
	keysAllowMissing bool   // --keys-allow-missing: skip 403/404 keys instead of failing
	s3Concurrency    int    // --s3-concurrency: HeadObject fan-out for the keys path
}

func (f *indexFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.indexFile, "index-file", "", "path to the index file to write")
	fl.BoolVar(&f.noSignRequest, "no-sign-request", false, "send anonymous requests (public buckets)")
	fl.BoolVar(&f.requesterPays, "requester-pays", false, "add the requester-pays header to every request")
	fl.StringVar(&f.endpoint, "endpoint", "", "override the S3 endpoint (S3-compatible stores)")
	fl.BoolVar(&f.pathStyle, "path-style", false, "use path-style addressing")
	fl.StringVar(&f.region, "region", "", "bucket region (resolved from the bucket if empty)")
	fl.StringSliceVar(&f.shards, "shard", nil, "sub-prefix to list concurrently (repeatable)")
	fl.BoolVar(&f.exec, "exec", false, "report files as mode 0555 instead of 0444")
	fl.Int32Var(&f.pageSize, "page-size", 1000, "ListObjectsV2 page size")
	fl.StringVar(&f.inventory, "inventory", "", "build from an S3 Inventory manifest.json instead of listing")
	fl.StringVar(&f.keys, "keys", "", "build from an explicit key list file (for LIST-denied buckets)")
	fl.StringVar(&f.keysFromManifest, "keys-from-manifest", "", "build from a key manifest (s3:// URL or local path, gz ok)")
	fl.BoolVar(&f.keysAllowMissing, "keys-allow-missing", false, "skip keys that HEAD reports as 403/404 instead of failing")
	fl.IntVar(&f.s3Concurrency, "s3-concurrency", 128, "HeadObject concurrency for the --keys build path")
	_ = cmd.MarkFlagRequired("index-file")
}

func (f *indexFlags) s3config(bucket string) s3client.Config {
	return s3client.Config{
		Bucket:        bucket,
		Region:        f.region,
		NoSignRequest: f.noSignRequest,
		RequesterPays: f.requesterPays,
		Endpoint:      f.endpoint,
		PathStyle:     f.pathStyle,
	}
}

func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build, refresh, and inspect the namespace index",
	}
	cmd.AddCommand(newIndexBuildCmd("build", "Build a new index"))
	cmd.AddCommand(newIndexBuildCmd("refresh", "Re-list the bucket and rewrite the index"))
	cmd.AddCommand(newIndexInspectCmd())
	return cmd
}

func newIndexBuildCmd(use, short string) *cobra.Command {
	var f indexFlags
	cmd := &cobra.Command{
		Use:   use + " s3://bucket[/prefix]",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, prefix, err := parseS3URL(args[0])
			if err != nil {
				return err
			}
			return runIndexBuild(cmd.Context(), cmd.OutOrStdout(), &f, bucket, prefix)
		},
	}
	f.bind(cmd)
	return cmd
}

func runIndexBuild(ctx context.Context, out io.Writer, f *indexFlags, bucket, prefix string) error {
	log := newLogger()

	usingKeys := f.keys != "" || f.keysFromManifest != ""
	if f.inventory != "" && usingKeys {
		return fmt.Errorf("--inventory and --keys/--keys-from-manifest are mutually exclusive")
	}
	if f.keys != "" && f.keysFromManifest != "" {
		return fmt.Errorf("--keys and --keys-from-manifest are mutually exclusive")
	}

	var (
		ix  *index.Index
		res index.KeysResult
		err error
	)
	switch {
	case f.inventory != "":
		ix, err = buildFromInventory(ctx, f, bucket, prefix, log)
	case usingKeys:
		ix, res, err = buildFromKeys(ctx, f, bucket, prefix, log)
	default:
		client, cerr := newS3Client(ctx, f.s3config(bucket))
		if cerr != nil {
			return cerr
		}
		ix, err = index.BuildFromList(ctx, client, index.ListOptions{
			Options:  index.Options{Bucket: bucket, Prefix: prefix, Exec: f.exec, Logger: log},
			Shards:   f.shards,
			PageSize: f.pageSize,
		})
	}
	if err != nil {
		return err
	}

	if err := ix.Save(f.indexFile); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	dropped, shadowed, collisions := ix.Stats()
	if _, err := fmt.Fprintf(out, "wrote %s: %d keys (dropped %d, shadowed %d, inode collisions %d)\n",
		f.indexFile, ix.Len(), dropped, shadowed, collisions); err != nil {
		return err
	}
	if usingKeys {
		_, err = fmt.Fprintf(out, "keys source: headed %d, missing %d\n", res.Headed, res.Missing)
	}
	return err
}

// buildFromKeys wires an explicit key-list source. The list is either --keys (a
// local path) or --keys-from-manifest (an s3:// URL or a local path, optionally
// gzipped). It reads the raw bytes, records their sha256 as provenance, then
// transparently gunzips, parses, and HeadObject-s the keys against the target
// bucket.
func buildFromKeys(ctx context.Context, f *indexFlags, bucket, prefix string, log *slog.Logger) (*index.Index, index.KeysResult, error) {
	src := f.keys
	source := "keys"
	if src == "" {
		src = f.keysFromManifest
		source = "manifest"
	}

	var raw io.ReadCloser
	if destBucket, key, perr := parseS3URL(src); perr == nil {
		client, cerr := newS3Client(ctx, f.s3config(destBucket))
		if cerr != nil {
			return nil, index.KeysResult{}, cerr
		}
		rc, gerr := client.GetObject(ctx, key, 0, 0)
		if gerr != nil {
			return nil, index.KeysResult{}, fmt.Errorf("fetch key list: %w", gerr)
		}
		raw = rc
	} else {
		file, oerr := os.Open(src)
		if oerr != nil {
			return nil, index.KeysResult{}, oerr
		}
		raw = file
	}
	defer func() { _ = raw.Close() }()

	// Read the raw (possibly-gzipped) bytes so the sha256 provenance covers
	// exactly what the operator supplied. Bounded to stay memory-safe.
	const maxRawBytes = 8 << 30
	data, rerr := io.ReadAll(io.LimitReader(raw, maxRawBytes+1))
	if rerr != nil {
		return nil, index.KeysResult{}, fmt.Errorf("read key list: %w", rerr)
	}
	if int64(len(data)) > maxRawBytes {
		return nil, index.KeysResult{}, fmt.Errorf("key list exceeds the size limit (%d bytes)", int64(maxRawBytes))
	}
	sum := sha256.Sum256(data)

	dec, derr := index.MaybeGunzip(bytes.NewReader(data))
	if derr != nil {
		return nil, index.KeysResult{}, fmt.Errorf("gunzip key list: %w", derr)
	}
	keys, perr := index.ParseKeyList(dec)
	if perr != nil {
		return nil, index.KeysResult{}, perr
	}

	client, cerr := newS3Client(ctx, f.s3config(bucket))
	if cerr != nil {
		return nil, index.KeysResult{}, cerr
	}
	return index.BuildFromKeys(ctx, client, index.KeysOptions{
		Options:      index.Options{Bucket: bucket, Prefix: prefix, Exec: f.exec, Logger: log, Source: source},
		Keys:         keys,
		Concurrency:  f.s3Concurrency,
		AllowMissing: f.keysAllowMissing,
		KeysSHA:      sum,
	})
}

// buildFromInventory wires an S3 Inventory manifest source. The manifest may be
// a local path or an s3:// URL; data files are fetched from the manifest's
// destination bucket.
func buildFromInventory(ctx context.Context, f *indexFlags, bucket, prefix string, log *slog.Logger) (*index.Index, error) {
	var (
		manifest index.Manifest
		open     func(context.Context, string) (io.ReadCloser, error)
		err      error
	)

	if destBucket, manifestKey, perr := parseS3URL(f.inventory); perr == nil {
		// Manifest and data files live in the inventory destination bucket.
		destClient, cerr := s3client.New(ctx, f.s3config(destBucket))
		if cerr != nil {
			return nil, cerr
		}
		rc, gerr := destClient.GetObject(ctx, manifestKey, 0, 0)
		if gerr != nil {
			return nil, fmt.Errorf("fetch manifest: %w", gerr)
		}
		manifest, err = index.ParseManifest(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		open = func(ctx context.Context, key string) (io.ReadCloser, error) {
			return destClient.GetObject(ctx, key, 0, 0)
		}
	} else {
		// Treat --inventory as a local manifest.json; data files are resolved
		// relative to the manifest's directory.
		file, oerr := os.Open(f.inventory)
		if oerr != nil {
			return nil, oerr
		}
		manifest, err = index.ParseManifest(file)
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		dir := filepath.Dir(f.inventory)
		open = func(_ context.Context, key string) (io.ReadCloser, error) {
			return os.Open(filepath.Join(dir, filepath.Base(key)))
		}
	}

	return index.BuildFromInventory(ctx, index.InventoryOptions{
		Options:  index.Options{Bucket: bucket, Prefix: prefix, Exec: f.exec, Logger: log},
		Manifest: manifest,
		Open:     open,
		GzipData: true,
	})
}

func newIndexInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect INDEX-FILE",
		Short: "Print statistics for a built index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ix, closeFn, err := index.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = closeFn() }()

			dropped, shadowed, collisions := ix.Stats()
			n := ix.Len()
			bytesPerKey := 0.0
			if n > 0 {
				bytesPerKey = float64(indexFileSize(args[0])) / float64(n)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "index-file:        %s\n", args[0])
			fmt.Fprintf(&b, "format-version:    %d\n", index.FormatVersion)
			fmt.Fprintf(&b, "bucket:            %s\n", ix.Bucket())
			root := ix.Prefix()
			if root == "" {
				root = "(bucket root)"
			}
			fmt.Fprintf(&b, "root:              %s\n", root)
			fmt.Fprintf(&b, "keys:              %d\n", n)
			fmt.Fprintf(&b, "bytes-per-key:     %.1f\n", bytesPerKey)
			fmt.Fprintf(&b, "source:            %s\n", ix.Source())
			if sha := ix.KeysSHAHex(); sha != "" {
				fmt.Fprintf(&b, "keys-sha256:       %s\n", sha)
			}
			fmt.Fprintf(&b, "dropped-keys:      %d\n", dropped)
			fmt.Fprintf(&b, "shadowed-keys:     %d\n", shadowed)
			fmt.Fprintf(&b, "inode-collisions:  %d\n", collisions)
			_, err = io.WriteString(cmd.OutOrStdout(), b.String())
			return err
		},
	}
}

func indexFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
