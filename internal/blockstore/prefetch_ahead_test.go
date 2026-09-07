// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
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

	ctx := context.Background()
	pf := prefetch.New(32)
	dispatch := func(blocks []int64) {
		for _, b := range blocks {
			b := b
			go bs.Prefetch(ctx, k, b, size)
		}
	}

	// Open dispatches the initial window (blocks 0,1) before any read.
	dispatch(pf.Open())
	time.Sleep(15 * time.Millisecond) // let the open-time GETs start
	if srv.GetCallCount() < 1 {
		t.Fatalf("expected GET(s) for blocks 0-1 issued at open; GetCalls=%d", srv.GetCallCount())
	}

	for d := int64(0); d <= 20; d++ {
		dispatch(pf.Observe(d))
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
