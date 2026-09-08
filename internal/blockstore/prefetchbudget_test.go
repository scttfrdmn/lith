// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// budgetRec counts S3 bytes fetched and prefetch eviction/accuracy events.
type budgetRec struct {
	bytes   atomic.Int64
	issued  atomic.Int64
	hit     atomic.Int64
	evicted atomic.Int64
}

func (r *budgetRec) MemHit()                {}
func (r *budgetRec) DiskHit()               {}
func (r *budgetRec) Miss()                  {}
func (r *budgetRec) StartInflight()         {}
func (r *budgetRec) EndInflight()           {}
func (r *budgetRec) StaleKey(string)        {}
func (r *budgetRec) UncoveredMiss()         {}
func (r *budgetRec) PrefetchIssued()        { r.issued.Add(1) }
func (r *budgetRec) PrefetchHit()           { r.hit.Add(1) }
func (r *budgetRec) PrefetchEvictedUnread() { r.evicted.Add(1) }
func (r *budgetRec) S3Get(n int64, isErr bool) {
	if !isErr {
		r.bytes.Add(n)
	}
}

// TestPrefetchBudgetNoThrash: 8 scripted sequential readers sharing a memory
// tier far smaller than their combined working set must not evict any
// prefetched-but-unread chunk (evicted_unread == 0), and must fetch each chunk
// exactly once (total S3 bytes == sum of file bytes) — no thrash, no re-fill
// (#55).
func TestPrefetchBudgetNoThrash(t *testing.T) {
	srv := fake.New()
	const nReaders = 8
	const nChunks = 96 // 96 MiB per object -> 768 MiB combined working set
	keys := make([]Key, nReaders)
	for i := 0; i < nReaders; i++ {
		name := "obj" + string(rune('A'+i))
		makeObj(srv, name, nChunks)
		keys[i] = keyFor(t, srv, name)
	}
	rec := &budgetRec{}
	// 512 MiB memory tier (8 chunks/shard across 64 shards), 64 MiB prefetch
	// budget: the 768 MiB combined working set forces eviction of already-read
	// chunks, but unread prefetch must be protected (evicted_unread == 0).
	bs := newStore(t, srv, Config{
		BlockSize: 8 << 20, MemCache: 512 * mib, PrefetchBudget: 64 * mib, Recorder: rec,
	})
	blk := bs.BlockChunks()
	nBlocks := int64(nChunks) / blk
	objSize := int64(nChunks) * mib

	var wg sync.WaitGroup
	for i := 0; i < nReaders; i++ {
		wg.Add(1)
		go func(k Key) {
			defer wg.Done()
			ctx := context.Background()
			for b := int64(0); b < nBlocks; b++ {
				// Prefetch several blocks ahead (over-eager — the budget must clamp it).
				for a := int64(0); a < 8; a++ {
					bs.Prefetch(ctx, k, b+a, objSize)
				}
				// Demand-read the current block's chunks.
				for ci := b * blk; ci < (b+1)*blk; ci++ {
					if _, err := bs.Chunk(ctx, k, ci, objSize); err != nil {
						t.Errorf("Chunk %d: %v", ci, err)
						return
					}
				}
			}
		}(keys[i])
	}
	wg.Wait()

	if ev := rec.evicted.Load(); ev != 0 {
		t.Errorf("evicted_unread = %d, want 0 (thrash)", ev)
	}
	if used := bs.pfBudget.used.Load(); used > bs.pfBudget.cap {
		t.Errorf("prefetch budget overshoot: used %d > cap %d", used, bs.pfBudget.cap)
	}
	wantBytes := int64(nReaders) * int64(nChunks) * mib
	if got := rec.bytes.Load(); got != wantBytes {
		t.Errorf("S3 bytes fetched = %d, want %d (each chunk once, no re-fill)", got, wantBytes)
	}
}

// TestPrefetchBudgetSingleReaderUnthrottled: with a budget larger than a single
// handle's readahead window, prefetching a full window succeeds — the budget
// only bites when aggregate readahead would exceed the memory tier.
func TestPrefetchBudgetSingleReaderUnthrottled(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 128)
	k := keyFor(t, srv, "obj")
	rec := &budgetRec{}
	bs := newStore(t, srv, Config{
		BlockSize: 8 << 20, MemCache: 512 * mib, PrefetchBudget: 256 * mib, Recorder: rec,
	})
	ctx := context.Background()
	objSize := int64(128) * mib
	// Prefetch a 64 MiB window (8 blocks) — well under the 256 MiB budget.
	for b := int64(0); b < 8; b++ {
		bs.Prefetch(ctx, k, b, objSize)
	}
	if got := rec.issued.Load(); got != 64 { // 8 blocks × 8 chunks
		t.Errorf("prefetch issued = %d, want 64 (full window, no budget throttle)", got)
	}
	if ev := rec.evicted.Load(); ev != 0 {
		t.Errorf("evicted_unread = %d, want 0", ev)
	}
}
