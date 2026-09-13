// SPDX-License-Identifier: Apache-2.0

// Package lithindex is the public, cross-repo entry point for building a lith
// index from a CargoShip 2.1 manifest. It exists so a producer (e.g.
// `cargoship publish`, github.com/scttfrdmn/cargoship) can build the mountable
// index inline as a library rather than shelling out to the `lith` binary —
// closing the seam between the tools without the auto-list footgun (lith#141) of
// a publish that silently ships no index. The heavy lifting lives in the
// internal index builder; this package is a thin, dependency-light adapter over
// a caller-supplied S3 read surface.
//
// As the one public, cross-repo entry point into lith's index machinery, this
// package's exported API (S3Fetcher, Options, BuildIndexFromManifest) is a
// compatibility surface for downstream modules and is deliberately kept minimal:
// prefer adding fields to Options over new functions, and do not widen S3Fetcher
// beyond the reads a manifest build actually performs.
package lithindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"

	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client"
)

// S3Fetcher is the minimal S3 read surface an index build needs: HEAD a key for
// its ETag (the immutability identity lith records per chunk) and GET an object's
// full bytes (only used to walk a previous manifest in an incremental chain).
// Callers implement it over their own S3 client. No listing is ever performed.
type S3Fetcher interface {
	// HeadETag returns the raw ETag of key (as S3 returns it, quotes included).
	HeadETag(ctx context.Context, key string) (etag string, err error)
	// GetObject returns a reader for the whole object at key. The caller closes it.
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
}

// Options is the subset of index-build options a manifest-driven build needs.
type Options struct {
	// Bucket the chunks and manifests live in (provenance only; keys are absolute).
	Bucket string
	// Exec makes files report mode 0555 instead of 0444.
	Exec bool
	// BuildTime is the fallback mtime for directories with no dated descendant;
	// zero means now.
	BuildTime time.Time
}

// BuildIndexFromManifest builds a lith index from a CargoShip 2.1 manifest and
// returns the serialized .lith image plus its sha256. manifestBytes is the raw
// manifest JSON (gzip transparent). The build HEADs each chunk the manifest names
// (for its ETag) and GETs any previous manifest in an incremental chain, all via
// fetch — never a list. The returned sha256 is of the image bytes, suitable for
// the `index_sha256` field of a CURRENT pointer.
func BuildIndexFromManifest(ctx context.Context, fetch S3Fetcher, manifestBytes []byte, opts Options) (image []byte, sha [32]byte, err error) {
	if fetch == nil {
		return nil, [32]byte{}, errors.New("lithindex: nil S3Fetcher")
	}
	raw, err := index.ReadManifestBytes(bytes.NewReader(manifestBytes))
	if err != nil {
		return nil, [32]byte{}, err
	}
	manSHA := sha256.Sum256(raw)
	ix, err := index.BuildFromCargoshipManifest(ctx, s3adapter{fetch}, index.CargoshipOptions{
		Options: index.Options{
			Bucket:    opts.Bucket,
			Exec:      opts.Exec,
			BuildTime: opts.BuildTime,
			Source:    "cargoship",
			KeysSHA:   manSHA,
		},
		ManifestBytes: raw,
		ManifestSHA:   manSHA,
	})
	if err != nil {
		return nil, [32]byte{}, err
	}
	image = ix.Marshal()
	return image, sha256.Sum256(image), nil
}

// s3adapter satisfies the internal s3client.API using only the two operations the
// manifest build performs. The listing/range methods are never called by the
// build path and return a clear error if they ever are.
type s3adapter struct{ f S3Fetcher }

var errUnsupported = errors.New("lithindex: operation not supported by the publish S3 adapter")

func (a s3adapter) HeadObject(ctx context.Context, key string) (s3client.Object, error) {
	etag, err := a.f.HeadETag(ctx, key)
	if err != nil {
		return s3client.Object{}, err
	}
	return s3client.Object{Key: key, ETag: etag}, nil
}

func (a s3adapter) GetObject(ctx context.Context, key string, _, _ int64) (io.ReadCloser, error) {
	// The build only GETs whole previous-manifest objects (off=0, length=0).
	return a.f.GetObject(ctx, key)
}

func (a s3adapter) ListObjectsV2(context.Context, string, string, int32) (s3client.ListPage, error) {
	return s3client.ListPage{}, errUnsupported
}

func (a s3adapter) GetRange(context.Context, string, int64, int64) ([]byte, string, error) {
	return nil, "", errUnsupported
}

func (a s3adapter) GetRangeReader(context.Context, string, int64, int64) (io.ReadCloser, string, error) {
	return nil, "", errUnsupported
}
