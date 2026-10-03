// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for the fixture's precondition")
}

// #320: three paths that consume or supersede a prefetched chunk without crediting it.
//
// All three were found by an external deployment reading this code after it retracted a
// 9-27 GB "leak" that turned out to be dispatch-side. Its own cells did not provoke any of
// them -- straddle was 0 and the demand/prefetch race never fired -- so these are the tests
// that decide whether they are real.
//
// The stakes are not only reporting. A consumed chunk left flagged unread is protected by the
// #55 eviction preference as if nothing had read it, so the tier keeps a dead chunk in
// preference to a live one. And lith_prefetch_unread_resident_bytes is the threshold an
// external measurement just validated 8/8 against the collapse condition (#313); if it
// over-reports on a straddle-heavy workload, that threshold reads high for the wrong reason.

// A: GetRange straddles two chunks that prefetch fetched. ensureChunks serves both from the
// tier and credits neither.
func TestStraddlingDemandReadCreditsThePrefetch(t *testing.T) {
	srv := fake.New()
	const nChunks = 8
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 128 * mib})
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	bs.Prefetch(ctx, k, 0, objSize)
	if got, want := bs.PrefetchCommittedBytes(), int64(nChunks)*mib; got != want {
		t.Fatalf("fixture: committed = %d, want %d", got, want)
	}
	unread0 := bs.PrefetchUnreadResidentBytes()
	if unread0 != int64(nChunks)*mib {
		t.Fatalf("fixture: unread resident = %d, want %d", unread0, int64(nChunks)*mib)
	}

	// A read STRADDLING chunks 0 and 1 — the GetRange path, not Chunk.
	if _, err := bs.GetRange(ctx, k, mib-4096, 8192, objSize); err != nil {
		t.Fatalf("GetRange: %v", err)
	}

	// Both chunks have now been read. Neither may still be charged as un-demanded prefetch,
	// and neither may still be flagged unread.
	if got, want := bs.PrefetchCommittedBytes(), int64(nChunks-2)*mib; got != want {
		t.Errorf("after a straddling read of chunks 0 and 1: committed = %d, want %d — a "+
			"demand read consumed two prefetched chunks and neither commitment was released",
			got, want)
	}
	if got, want := bs.PrefetchUnreadResidentBytes(), int64(nChunks-2)*mib; got != want {
		t.Errorf("after a straddling read: unread resident = %d, want %d — the chunks are "+
			"still flagged unread, so the #55 eviction preference protects chunks that have "+
			"been read", got, want)
	}
}

// A, second path: the same read served by a JOIN rather than a tier hit. ensureChunks waits on
// an in-flight fill and returns its data without crediting.
func TestDemandReadJoiningAPrefetchFillCreditsIt(t *testing.T) {
	srv := fake.New()
	const nChunks = 8
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	release := make(chan struct{})
	blocked := &blockingSource{Source: srv, release: release}
	bs, err := New(blocked, Config{Bucket: "bkt", BlockSize: 8 << 20, MemCache: 128 * mib})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(bs.Close)
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	go bs.Prefetch(ctx, k, 0, objSize)
	// Wait for the prefetch to have committed, so the demand read below joins it.
	waitFor(t, func() bool { return bs.PrefetchCommittedBytes() == int64(nChunks)*mib })

	done := make(chan error, 1)
	go func() {
		_, err := bs.GetRange(ctx, k, 0, 4096, objSize)
		done <- err
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("GetRange: %v", err)
	}

	if got, want := bs.PrefetchCommittedBytes(), int64(nChunks-1)*mib; got > want {
		t.Errorf("after a demand read joined the prefetch fill of chunk 0: committed = %d, "+
			"want <= %d — joining a prefetch fill did not credit it", got, want)
	}
}

// B: a DEMAND fill lands a chunk a concurrent Prefetch had already committed. It merges
// without an unread flag, and the demand owner never credits, so the commitment is charged
// forever with nothing resident to match it. This is the shape that would produce a genuine
// committed-only leak.
func TestDemandFillSupersedingAPrefetchCommitmentReleasesIt(t *testing.T) {
	srv := fake.New()
	const nChunks = 8
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 128 * mib})
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	// Commit the block without filling it: mark the chunks prefetched by hand, which is what
	// Prefetch does before its fill lands. Then let a DEMAND read fill chunk 0.
	for ci := int64(0); ci < nChunks; ci++ {
		ck := bs.cacheKey(k, ci)
		n := chunkLenOf(ci, objSize)
		bs.pfCommittedBytes.Add(n)
		bs.prefetched.Store(ck, n)
	}
	if got, want := bs.PrefetchCommittedBytes(), int64(nChunks)*mib; got != want {
		t.Fatalf("fixture: committed = %d, want %d", got, want)
	}

	if _, err := bs.GetRange(ctx, k, 0, 4096, objSize); err != nil {
		t.Fatalf("GetRange: %v", err)
	}

	// Chunk 0 was fetched by demand, so any prefetch commitment on it is void. It is resident
	// and read; nothing will ever evict-unread it or credit it, so if the commitment survives
	// here it survives forever.
	if got, want := bs.PrefetchCommittedBytes(), int64(nChunks-1)*mib; got != want {
		t.Errorf("after a demand fill landed a chunk prefetch had committed: committed = %d, "+
			"want %d — the commitment is charged with nothing resident to match it, and no "+
			"path will ever release it", got, want)
	}
	if got := bs.PrefetchUnreadResidentBytes(); got != 0 {
		t.Errorf("unread resident = %d, want 0: a demand fill must not flag its chunk unread", got)
	}
}

// C: a PREFETCH that finds its chunk already cached must not credit itself a hit. Crediting
// clears the unread flag, which hands the chunk to the #55 eviction preference as "already
// read" when nothing has read it, and releases a budget reservation that is still owed.
func TestPrefetchConsumingItsOwnChunkDoesNotCreditAHit(t *testing.T) {
	srv := fake.New()
	const nChunks = 8
	makeObj(srv, "obj", nChunks)
	k := keyFor(t, srv, "obj")
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MemCache: 128 * mib})
	ctx := context.Background()
	objSize := int64(nChunks) * mib

	bs.Prefetch(ctx, k, 0, objSize)
	committed0 := bs.PrefetchCommittedBytes()
	unread0 := bs.PrefetchUnreadResidentBytes()
	if committed0 == 0 || unread0 == 0 {
		t.Fatalf("fixture: committed=%d unread=%d, both must be non-zero", committed0, unread0)
	}

	// fetchExtents on the prefetch path, for a chunk prefetch already landed.
	if _, err := bs.fetchExtents(ctx, k, 0, fullExtents, objSize, true, fillWhole); err != nil {
		t.Fatalf("fetchExtents: %v", err)
	}

	if got := bs.PrefetchCommittedBytes(); got != committed0 {
		t.Errorf("a prefetch re-reading its own chunk changed committed from %d to %d: it "+
			"credited itself a demand hit", committed0, got)
	}
	if got := bs.PrefetchUnreadResidentBytes(); got != unread0 {
		t.Errorf("a prefetch re-reading its own chunk changed unread-resident from %d to %d: "+
			"it cleared the unread flag, so #55 will now evict a chunk nothing has read",
			unread0, got)
	}
}
