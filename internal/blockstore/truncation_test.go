// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// truncatingSource wraps a fake.Server and, while truncate is set, hands back a
// body that delivers FEWER bytes than requested (the first half of the range,
// then EOF) — simulating a dropped/partial transfer. This exercises H1: a short
// per-chunk read must be treated as a truncation failure, never cached and
// served as zero-padded "valid" data.
type truncatingSource struct {
	*fake.Server
	truncate atomic.Bool
}

func (t *truncatingSource) GetRangeReader(ctx context.Context, key string, off, length int64) (io.ReadCloser, string, error) {
	body, etag, err := t.Server.GetRangeReader(ctx, key, off, length)
	if err != nil || !t.truncate.Load() {
		return body, etag, err
	}
	all, rerr := io.ReadAll(body)
	_ = body.Close()
	if rerr != nil {
		return nil, "", rerr
	}
	short := all
	if len(all) > 0 {
		short = all[:len(all)/2] // deliver half the bytes, then EOF
	}
	return io.NopCloser(bytes.NewReader(short)), etag, nil
}

// TestTruncatedChunkNotCachedAsZeros verifies H1: when an S3 body is truncated,
// the demand read fails instead of caching a zero-padded chunk, and a later
// (untruncated) read then serves the correct bytes rather than cached zeros.
func TestTruncatedChunkNotCachedAsZeros(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	ts := &truncatingSource{Server: srv}
	ts.truncate.Store(true)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib

	bs, err := New(ts, Config{Bucket: "bkt", BlockSize: 8 << 20, MaxRange: 64 << 20, MemCache: 256 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Read chunk 1 (every byte == 1), so "correct bytes" are distinguishable from
	// the zero-padded buffer a truncation bug would cache and serve.
	const ci = int64(1)

	// (a) A truncated body must make the demand read fail, not succeed with zeros.
	if _, err := bs.GetRange(context.Background(), k, ci*mib, 4096, size); err == nil {
		t.Fatal("truncated read returned nil error; a short read must fail (H1)")
	}

	// (b) The truncated chunk must not have been cached: nothing in any tier.
	if _, tier := bs.lookup(k, ci); tier != "" {
		t.Fatalf("truncated chunk was cached in tier %q; it must not be served as authoritative", tier)
	}

	// Stop truncating; a retry must now serve the real bytes (chunk 1 == byte 1),
	// proving no zero-padded buffer was cached and returned instead.
	ts.truncate.Store(false)
	data, err := bs.GetRange(context.Background(), k, ci*mib, 4096, size)
	if err != nil {
		t.Fatalf("retry after truncation: %v", err)
	}
	if data[0] != byte(ci) {
		t.Errorf("retry served byte %d, want %d (correct chunk-1 data, not cached zeros)", data[0], byte(ci))
	}
}
