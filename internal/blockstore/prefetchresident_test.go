// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// #301: --prefetch-budget bounds bytes prefetched and still resident unread, and that
// quantity was nowhere — the budget was enforced by a proxy (per-handle window x open
// handles) and the proxy was all anyone could observe.
//
// This asserts the real accounting against CONSTRUCTED state, deliberately not against
// the proxy. The reporting workload demonstrated that an estimator built from dispatch
// counts returns the standing window by construction, so "resident ~= window x handles"
// is an identity that passes whether or not either number is right; they nearly published
// a conclusion from it. Every number below is a chunk count this test caused.
func TestPrefetchResidentBytesTracksTheRealSet(t *testing.T) {
	srv := fake.New()
	const nChunks = 16
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 256 * mib})
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	if got := bs.PrefetchResidentBytes(); got != 0 {
		t.Fatalf("fresh store: resident = %d, want 0", got)
	}

	// Prefetch block 0 — 8 chunks of 1 MiB — and nothing has consumed them.
	bs.Prefetch(ctx, k, 0, objSize)
	if got, want := bs.PrefetchResidentBytes(), int64(8)*mib; got != want {
		t.Errorf("after prefetching one 8-chunk block: resident = %d, want %d", got, want)
	}

	// A demand read of the first chunk consumes exactly that chunk's worth.
	if _, err := bs.Chunk(ctx, k, 0, objSize, 0, mib, true); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if got, want := bs.PrefetchResidentBytes(), int64(7)*mib; got != want {
		t.Errorf("after consuming one chunk: resident = %d, want %d", got, want)
	}

	// Re-prefetching the same block must not double-count: every chunk is either
	// already resident-unread or already consumed.
	bs.Prefetch(ctx, k, 0, objSize)
	if got, want := bs.PrefetchResidentBytes(), int64(7)*mib; got != want {
		t.Errorf("after re-prefetching the same block: resident = %d, want %d (no double count)", got, want)
	}

	// Draining the rest returns the total to zero, which is the property that makes the
	// gauge trustworthy: the three removal paths must between them account for every
	// chunk the add path counted.
	for ci := int64(1); ci < 8; ci++ {
		if _, err := bs.Chunk(ctx, k, ci, objSize, ci*mib, ci*mib+mib, true); err != nil {
			t.Fatalf("Chunk %d: %v", ci, err)
		}
	}
	if got := bs.PrefetchResidentBytes(); got != 0 {
		t.Errorf("after consuming every prefetched chunk: resident = %d, want 0", got)
	}
}

// The short trailing chunk must be counted at its real length, not a full chunk. The old
// map value was struct{}{}, so the length had to be introduced to make the removal sites
// exact — and a trailing chunk is where an off-by-one would hide.
func TestPrefetchResidentBytesCountsAShortTrailingChunk(t *testing.T) {
	srv := fake.New()
	// 2 chunks and a bit: the last chunk is short.
	const objSize = int64(2)*mib + 123456
	srv.Put("short", make([]byte, objSize), time.Unix(1, 0))
	k := keyFor(t, srv, "short")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 256 * mib})

	bs.Prefetch(context.Background(), k, 0, objSize)
	if got := bs.PrefetchResidentBytes(); got != objSize {
		t.Errorf("resident = %d, want the object's %d — a short trailing chunk must count "+
			"its real length, not a full chunk", got, objSize)
	}
}
