// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// countRec is a Recorder that tallies the events relevant to #38.
type countRec struct {
	uncovered int64
	prefetch  int64
}

func (c *countRec) MemHit()           {}
func (c *countRec) DiskHit()          {}
func (c *countRec) Miss()             {}
func (c *countRec) S3Get(int64, bool) {}
func (c *countRec) StartInflight()    {}
func (c *countRec) EndInflight()      {}
func (c *countRec) StaleKey(string)   {}
func (c *countRec) PrefetchIssued()   { atomic.AddInt64(&c.prefetch, 1) }
func (c *countRec) PrefetchHit()      {}
func (c *countRec) UncoveredMiss()    { atomic.AddInt64(&c.uncovered, 1) }
func (c *countRec) uncov() int64      { return atomic.LoadInt64(&c.uncovered) }

// TestPrefetchAheadCoversDemand drives the same open+read+prefetch sequence the
// FUSE layer does over a 64-block file (1 block = 1 chunk here) and asserts:
//   - GETs for blocks 0–1 are issued at open, before the first demand read;
//   - no demand read after block 2 is an uncovered miss (the frontier led it).
func TestPrefetchAheadCoversDemand(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 64)
	srv.GetDelay = 25 * time.Millisecond // keep fills in flight across a read
	k := keyFor(t, srv, "obj")
	size := int64(64) * mib
	rec := &countRec{}
	// 1 MiB block so block index == chunk index (clean 1:1 for the assertion).
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})

	// Synchronize on GETs actually starting instead of sleeping: by the time
	// GetRangeReader is called, ensureChunks has already claimed the run's
	// chunks in-flight, so waiting for the frontier's GETs to start guarantees
	// the frontier leads the cursor — with no dependence on goroutine-scheduling
	// timing (deterministic under -race -count=N).
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	started := map[int64]bool{}
	srv.OnGetStart = func(_ string, off, _ int64) {
		mu.Lock()
		started[off/mib] = true
		cond.Broadcast()
		mu.Unlock()
	}
	waitStarted := func(blocks []int64) {
		mu.Lock()
		defer mu.Unlock()
		for {
			all := true
			for _, b := range blocks {
				if b*mib >= size {
					continue // no GET is issued for a block past EOF
				}
				if !started[b] {
					all = false
					break
				}
			}
			if all {
				return
			}
			cond.Wait()
		}
	}

	ctx := context.Background()
	pf := prefetch.New(32)
	dispatch := func(blocks []int64) {
		for _, b := range blocks {
			b := b
			go bs.Prefetch(ctx, k, b, size)
		}
	}

	// Open dispatches the initial window before any read; wait until those GETs
	// are in flight.
	ob := pf.Open()
	dispatch(ob)
	waitStarted(ob)
	if srv.GetCallCount() < 1 {
		t.Fatalf("expected GET(s) for the open window issued at open; GetCalls=%d", srv.GetCallCount())
	}

	for d := int64(0); d <= 20; d++ {
		nb := pf.Observe(d)
		dispatch(nb)
		waitStarted(nb) // frontier GETs are in flight before we read behind them
		before := rec.uncov()
		if _, err := bs.GetRange(ctx, k, d*mib, 4096, size); err != nil {
			t.Fatalf("read block %d: %v", d, err)
		}
		if d >= 2 && rec.uncov() != before {
			t.Errorf("demand read for block %d was an uncovered miss (frontier did not lead)", d)
		}
	}
}

// TestUncoveredMissWithoutPrefetch is the control: with no prefetcher driving,
// every demand read originates its own fetch and is counted uncovered.
func TestUncoveredMissWithoutPrefetch(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	size := int64(8) * mib
	rec := &countRec{}
	bs := newStore(t, srv, Config{BlockSize: 1 << 20, MaxRange: 64 << 20, Recorder: rec})

	for d := int64(0); d < 4; d++ {
		if _, err := bs.GetRange(context.Background(), k, d*mib, 4096, size); err != nil {
			t.Fatal(err)
		}
	}
	if rec.uncov() != 4 {
		t.Errorf("uncovered misses = %d, want 4 (no prefetch)", rec.uncov())
	}
}
