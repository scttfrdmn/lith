// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"io"
	"sync"
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
func TestPrefetchCommittedBytesTracksTheRealSet(t *testing.T) {
	srv := fake.New()
	const nChunks = 16
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 256 * mib})
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	if got := bs.PrefetchCommittedBytes(); got != 0 {
		t.Fatalf("fresh store: resident = %d, want 0", got)
	}

	// Prefetch block 0 — 8 chunks of 1 MiB — and nothing has consumed them.
	bs.Prefetch(ctx, k, 0, objSize)
	if got, want := bs.PrefetchCommittedBytes(), int64(8)*mib; got != want {
		t.Errorf("after prefetching one 8-chunk block: resident = %d, want %d", got, want)
	}

	// A demand read of the first chunk consumes exactly that chunk's worth.
	if _, err := bs.Chunk(ctx, k, 0, objSize, 0, mib, true); err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if got, want := bs.PrefetchCommittedBytes(), int64(7)*mib; got != want {
		t.Errorf("after consuming one chunk: resident = %d, want %d", got, want)
	}

	// Re-prefetching the same block must not double-count: every chunk is either
	// already resident-unread or already consumed.
	bs.Prefetch(ctx, k, 0, objSize)
	if got, want := bs.PrefetchCommittedBytes(), int64(7)*mib; got != want {
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
	if got := bs.PrefetchCommittedBytes(); got != 0 {
		t.Errorf("after consuming every prefetched chunk: resident = %d, want 0", got)
	}
}

// The short trailing chunk must be counted at its real length, not a full chunk. The old
// map value was struct{}{}, so the length had to be introduced to make the removal sites
// exact — and a trailing chunk is where an off-by-one would hide.
func TestPrefetchCommittedBytesCountsAShortTrailingChunk(t *testing.T) {
	srv := fake.New()
	// 2 chunks and a bit: the last chunk is short.
	const objSize = int64(2)*mib + 123456
	srv.Put("short", make([]byte, objSize), time.Unix(1, 0))
	k := keyFor(t, srv, "short")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 256 * mib})

	bs.Prefetch(context.Background(), k, 0, objSize)
	if got := bs.PrefetchCommittedBytes(); got != objSize {
		t.Errorf("resident = %d, want the object's %d — a short trailing chunk must count "+
			"its real length, not a full chunk", got, objSize)
	}
}

// The semantics the name has to carry, flagged by the reporting workload after an earlier
// version called this "resident": the marking loop runs BEFORE ensureChunks, so a chunk
// counts from DISPATCH and keeps counting while its GET is in flight with nothing in RAM.
// That is defensible for admission control and indefensible as a memory figure, and the
// risk they named is that someone enforces a RAM limit with it.
//
// Asserted by holding the fetch open: the counter must already include the chunk while
// the source has not yet returned a byte.
func TestPrefetchCommittedBytesCountsBytesStillInFlight(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	objSize := int64(8) * mib

	release := make(chan struct{})
	blocked := &blockingSource{Source: srv, release: release}
	bs, err := New(blocked, Config{Bucket: "bkt", BlockSize: 8 << 20, MemCache: 256 * mib})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(bs.Close)

	done := make(chan struct{})
	go func() { defer close(done); bs.Prefetch(context.Background(), k, 0, objSize) }()

	// The fetch is blocked in the source, so nothing can be in RAM. The counter must
	// nonetheless already carry the full block: it counts commitments, not residency.
	deadline := time.Now().Add(2 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		if got = bs.PrefetchCommittedBytes(); got == int64(8)*mib {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if want := int64(8) * mib; got != want {
		t.Errorf("with every GET blocked in the source: committed = %d, want %d — the counter "+
			"must include bytes in flight, which is why it is not named 'resident'", got, want)
	}
	close(release)
	<-done
}

// blockingSource holds every range read until release is closed, so a test can observe
// the committed-bytes counter while nothing has arrived in RAM.
type blockingSource struct {
	Source
	release chan struct{}
}

func (b *blockingSource) GetRangeReader(ctx context.Context, key string, off, length int64) (io.ReadCloser, string, error) {
	<-b.release
	return b.Source.GetRangeReader(ctx, key, off, length)
}

// #301: admission is atomic with the accounting, which is the property a read-then-decide
// check would not have. N handles establishing simultaneously must not each read a stale zero
// and all grant themselves a full window -- that is the over-commitment that thrashes, and
// the reporting workload measured establishment as a single-shot full-window burst, so
// simultaneity is the normal case rather than a corner.
func TestAdmitCommittedIsAtomicUnderConcurrency(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 64)
	k := keyFor(t, srv, "obj")
	objSize := int64(64) * mib
	// A budget of 8 chunks against 64 chunks of demand: most dispatches must be refused.
	const budget = int64(8) * mib
	rec := &budgetRec{}
	bs := newStore(t, srv, Config{
		BlockSize: 1 << 20, MemCache: 64 * mib, PrefetchBudget: budget, Recorder: rec,
	})

	var wg sync.WaitGroup
	for b := int64(0); b < 64; b++ {
		wg.Add(1)
		go func(b int64) { defer wg.Done(); bs.Prefetch(context.Background(), k, b, objSize) }(b)
	}
	wg.Wait()

	if got := bs.PrefetchCommittedBytes(); got > budget {
		t.Errorf("committed = %d after 64 concurrent dispatches against a %d budget: admission "+
			"must be atomic with the accounting, or every dispatch reads a stale zero", got, budget)
	}
	if rec.issued.Load() > budget/mib {
		t.Errorf("issued = %d, more chunks than the budget could hold (%d) — refusals are not "+
			"being counted as refusals", rec.issued.Load(), budget/mib)
	}
}

// A refusal is not a wasted fetch and must not be counted as one. Conflating them would make
// lith_prefetch_issued_total include prefetches that never happened, breaking the
// issued-vs-used relation every #256 gate reads off.
func TestRefusedPrefetchIsNotCountedAsIssued(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 32)
	k := keyFor(t, srv, "obj")
	objSize := int64(32) * mib
	const budget = int64(4) * mib
	rec := &budgetRec{}
	bs := newStore(t, srv, Config{
		BlockSize: 1 << 20, MemCache: 64 * mib, PrefetchBudget: budget, Recorder: rec,
	})
	for b := int64(0); b < 32; b++ {
		bs.Prefetch(context.Background(), k, b, objSize)
	}
	issued := rec.issued.Load()
	if issued == 0 {
		t.Fatal("nothing issued at all: the fixture is not exercising admission")
	}
	if issued > budget/mib {
		t.Errorf("issued = %d against a budget of %d chunks: a refused prefetch was counted as "+
			"issued, which inflates lith_prefetch_issued_total with fetches that never happened",
			issued, budget/mib)
	}
}
