// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestPlanRangeFetchesOneExtent: a plan range of 40 KiB inside a chunk fetches
// exactly one 64 KiB extent, byte-exact (#118).
func TestPlanRangeFetchesOneExtent(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	// Plan range [12 MiB, 12 MiB+40 KiB): one extent.
	bs.FillRange(context.Background(), k, 12*mib, 12*mib+40<<10, size)
	if srv.GetBytes != ExtentSize {
		t.Fatalf("plan range fetched %d bytes, want %d (one extent)", srv.GetBytes, ExtentSize)
	}

	// A demand read of an adjacent, unfilled extent fetches only that extent.
	before := srv.GetBytes
	if _, err := bs.GetRange(context.Background(), k, 12*mib+ExtentSize, 100, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes-before != ExtentSize {
		t.Fatalf("adjacent demand read fetched %d bytes, want %d", srv.GetBytes-before, ExtentSize)
	}
	// A demand read inside the plan-filled extent adds nothing (served from cache).
	before = srv.GetBytes
	if _, err := bs.GetRange(context.Background(), k, 12*mib+1000, 100, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes != before {
		t.Fatalf("read of plan-filled extent fetched %d extra bytes, want 0", srv.GetBytes-before)
	}
}

// TestSequentialDemandFillsWholeChunk: a sequential-mode demand read (Chunk with
// sequential=true) fills the whole chunk — streaming is unchanged (#118).
func TestSequentialDemandFillsWholeChunk(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	if _, err := bs.Chunk(context.Background(), k, 5, size, 0, 4096, true); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes != mib {
		t.Fatalf("sequential demand fetched %d bytes, want %d (whole chunk)", srv.GetBytes, mib)
	}
}

// TestDiskRoundTripPreservesBitmap: a partial chunk persisted to disk comes back
// with its filled bitmap; the covered extents are served without a new GET, an
// uncovered one triggers a fetch (#118).
func TestDiskRoundTripPreservesBitmap(t *testing.T) {
	dir := t.TempDir()
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, DiskCache: 64 << 20, DiskPath: dir})

	bs.FillRange(context.Background(), k, 12*mib, 12*mib+40<<10, size) // extent 0 of chunk 12
	bs.Flush()

	// Drop the memory tier by making a fresh store over the same disk dir.
	bs2 := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, DiskCache: 64 << 20, DiskPath: dir})
	srv.GetBytes = 0
	if _, err := bs2.GetRange(context.Background(), k, 12*mib+1000, 100, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes != 0 {
		t.Fatalf("read of disk-persisted extent fetched %d bytes, want 0 (bitmap lost?)", srv.GetBytes)
	}
	if _, err := bs2.GetRange(context.Background(), k, 12*mib+ExtentSize, 100, size); err != nil {
		t.Fatal(err)
	}
	if srv.GetBytes != ExtentSize {
		t.Fatalf("read of uncovered extent fetched %d bytes, want %d", srv.GetBytes, ExtentSize)
	}
}

// TestExtentJoinsWholeChunkOneGet: a whole-chunk fill in flight and a concurrent
// extent fill of the same chunk join into a single GET (singleflight is per
// chunk; an extent fill joins/extends the in-flight fill rather than racing it).
func TestExtentJoinsWholeChunkOneGet(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 40)
	k := keyFor(t, srv, "obj")
	size := int64(40) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})
	srv.GetDelay = 80 * time.Millisecond // widen the in-flight window so the join is deterministic

	// Start the whole-chunk fill first so it owns the chunk; while its (slow) GET
	// is in flight, an extent fill of the same chunk joins it — one GET total.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = bs.Chunk(context.Background(), k, 7, size, 0, 4096, true) }()
	time.Sleep(20 * time.Millisecond) // let the whole-chunk fill claim + enter its GET
	bs.FillRange(context.Background(), k, 7*mib+2<<10, 7*mib+3<<10, size)
	wg.Wait()

	if srv.GetCalls != 1 {
		t.Fatalf("GetCalls=%d, want 1 (extent fill joined the whole-chunk fill)", srv.GetCalls)
	}
}

// TestConcurrentExtentFillsRace exercises the singleflight/merge path under -race.
func TestConcurrentExtentFillsRace(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	size := int64(8) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * ExtentSize // extents of chunk 0..1
			bs.FillRange(context.Background(), k, off, off+100, size)
			_, _ = bs.GetRange(context.Background(), k, off, 100, size)
		}(i)
	}
	wg.Wait()
}

// TestFillBatchCoalescesAcrossChunks (#124): 6 ranges spanning 3 cache chunks
// with 100 KiB gaps (< the 256 KiB coalesce gap) merge into one range GET;
// the gap bytes are counted.
func TestFillBatchCoalescesAcrossChunks(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 256 << 10})
	const kb = 1024
	ranges := []Range{
		{0, 300 * kb}, {400 * kb, 700 * kb}, {800 * kb, 1100 * kb},
		{1200 * kb, 1500 * kb}, {1600 * kb, 1900 * kb}, {2000 * kb, 2300 * kb},
	}
	bs.FillBatch(context.Background(), k, ranges, size)
	if srv.GetCalls != 1 {
		t.Fatalf("GetCalls=%d, want 1 (6 ranges / 3 chunks / 100 KiB gaps coalesced)", srv.GetCalls)
	}
	// The whole coalesced run [0, 2300 KiB) is fetched (extent-aligned).
	if srv.GetBytes < 2300*kb {
		t.Fatalf("fetched %d bytes, want >= the coalesced run", srv.GetBytes)
	}
}

// TestFillBatchGapBreaksRun (#124): two ranges 1 MiB apart (> coalesce gap) are
// two separate GETs.
func TestFillBatchGapBreaksRun(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 256 << 10})
	bs.FillBatch(context.Background(), k, []Range{{0, 200 << 10}, {1400 << 10, 1600 << 10}}, size)
	if srv.GetCalls != 2 {
		t.Fatalf("GetCalls=%d, want 2 (runs 1 MiB apart)", srv.GetCalls)
	}
}

// TestFillBatchJoinsInflight (#124): a batch whose extents are already in flight
// joins the in-flight fill rather than re-fetching.
func TestFillBatchJoinsInflight(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 4)
	k := keyFor(t, srv, "obj")
	size := int64(4) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 256 << 10})
	srv.GetDelay = 60 * time.Millisecond
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); bs.FillBatch(context.Background(), k, []Range{{0, 300 << 10}}, size) }()
	time.Sleep(20 * time.Millisecond)                                    // let the first batch's GET get in flight
	bs.FillBatch(context.Background(), k, []Range{{0, 300 << 10}}, size) // same extents
	wg.Wait()
	if srv.GetCalls != 1 {
		t.Fatalf("GetCalls=%d, want 1 (second batch joined the in-flight fill)", srv.GetCalls)
	}
}

// TestFillBatchRace exercises concurrent batches over one object under -race.
func TestFillBatchRace(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	size := int64(8) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 256 << 10})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * (300 << 10)
			bs.FillBatch(context.Background(), k, []Range{{off, off + 200<<10}}, size)
			_, _ = bs.GetRange(context.Background(), k, off, 100, size)
		}(i)
	}
	wg.Wait()
}

// TestDeriveCoalesceGap (#31): the gap is bandwidth × TTFB / C, clamped to
// [256 KiB, 64 MiB] — a round-trip amortized across C concurrent requests.
func TestDeriveCoalesceGap(t *testing.T) {
	cases := []struct {
		bps  int64
		ttfb time.Duration
		c    int
		want int64 // 0 = "≈, check band"
		band [2]int64
	}{
		{1_900_000_000, 40 * time.Millisecond, 128, 0, [2]int64{560_000, 640_000}},     // ~594 KB
		{117_000_000, 40 * time.Millisecond, 64, 256 << 10, [2]int64{}},                // ~73 KB → floor
		{1_900_000_000, 40 * time.Millisecond, 2, 0, [2]int64{37_000_000, 39_000_000}}, // C=2 → ~38 MB
		{1_900_000_000, 40 * time.Millisecond, 1, 64 << 20, [2]int64{}},                // C=1 → 76 MB → ceil
		{0, 40 * time.Millisecond, 128, 256 << 10, [2]int64{}},                         // unknown bw → floor
		{1_000_000_000, 0, 128, 256 << 10, [2]int64{}},                                 // unknown ttfb → floor
		{1_000_000_000, 40 * time.Millisecond, 0, 256 << 10, [2]int64{}},               // C=0 → floor
	}
	for _, c := range cases {
		got := deriveCoalesceGap(c.bps, c.ttfb, c.c)
		if c.want != 0 {
			if got != c.want {
				t.Errorf("derive(%d,%v,%d)=%d, want %d", c.bps, c.ttfb, c.c, got, c.want)
			}
			continue
		}
		if got < c.band[0] || got > c.band[1] {
			t.Errorf("derive(%d,%v,%d)=%d, want in %v", c.bps, c.ttfb, c.c, got, c.band)
		}
	}
}

// TestCoalesceGapUsesDevice (#31): CoalesceGap() is the full-concurrency gap
// (NIC × TTFB / prefetchConc). Override pins it.
func TestCoalesceGapUsesDevice(t *testing.T) {
	srv := fake.New()
	// prefetchConc defaults to S3Concurrency; set it to 128 for a known divisor.
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, S3Concurrency: 128,
		NICBytesPerSec: 1_900_000_000, TTFB: 40 * time.Millisecond})
	// 1.9 GB/s × 40 ms / 128 ≈ 594 KB (byte-precise, not a whole-file 64 MiB).
	if got := bs.CoalesceGap(); got < 560_000 || got > 640_000 {
		t.Fatalf("derived gap = %d, want ~594 KB", got)
	}
	bs2 := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20,
		NICBytesPerSec: 1_900_000_000, TTFB: 40 * time.Millisecond, CoalesceGap: 1 << 20})
	if got := bs2.CoalesceGap(); got != 1<<20 {
		t.Fatalf("override gap = %d, want 1 MiB", got)
	}
}

// TestFillBatchGapControlsGETs (#124/session 30): a projection spanning the file
// coalesces to few GETs under a large gap and stays per-chunk under a small one.
func TestFillBatchGapControlsGETs(t *testing.T) {
	// A projection: one 100 KiB range every 1 MiB across an 8 MiB object (8 ranges).
	mk := func() []Range {
		var rs []Range
		for i := int64(0); i < 8; i++ {
			rs = append(rs, Range{i * mib, i*mib + 100<<10})
		}
		return rs
	}
	// Large gap (64 MiB): all 8 ranges within one file → one run → one GET.
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	bsBig := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 64 << 20})
	bsBig.FillBatch(context.Background(), k, mk(), 8*mib)
	if srv.GetCalls != 1 {
		t.Fatalf("64 MiB gap: GetCalls=%d, want 1", srv.GetCalls)
	}
	// Small gap (256 KiB): the 1 MiB spacing exceeds it → each range its own run.
	srv2 := fake.New()
	makeObj(srv2, "obj", 8)
	k2 := keyFor(t, srv2, "obj")
	bsSmall := newStore(t, srv2, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 256 << 10})
	bsSmall.FillBatch(context.Background(), k2, mk(), 8*mib)
	if srv2.GetCalls != 8 {
		t.Fatalf("256 KiB gap: GetCalls=%d, want 8 (per-range)", srv2.GetCalls)
	}
}

// TestGatherDemandBatchesBurst (#124/session 30): concurrent footer demand misses
// on one object within the tick are collected into one coalesced FillBatch — a
// large gap merges the burst into a single GET.
func TestGatherDemandBatchesBurst(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 8)
	k := keyFor(t, srv, "obj")
	size := int64(8) * mib
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20, CoalesceGap: 64 << 20})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * mib
			bs.GatherDemand(context.Background(), k, off, 100<<10, size)
		}(i)
	}
	wg.Wait()
	// The burst should collapse into one batch (one coalesced GET); allow a single
	// straggler that registered after the leader's tick under a loaded scheduler.
	if srv.GetCalls > 2 {
		t.Fatalf("GetCalls=%d, want ≤2 (burst coalesced into one batch)", srv.GetCalls)
	}
}

// TestFillBatchParallelDispatch (#31): a many-run batch dispatches its runs
// concurrently (no per-run serialization) — the in-flight high-water mark reaches
// the pool/run bound, not 1.
func TestFillBatchParallelDispatch(t *testing.T) {
	srv := fake.New()
	makeObj(srv, "obj", 64)
	k := keyFor(t, srv, "obj")
	size := int64(64) * mib
	// Pool 128, tiny override gap so 64 ranges stay 64 separate runs.
	bs := newStore(t, srv, Config{BlockSize: 8 << 20, MaxRange: 64 << 20,
		S3Concurrency: 128, CoalesceGap: 64 << 10})
	srv.GetDelay = 60 * time.Millisecond // widen the window so runs overlap
	var ranges []Range
	for i := int64(0); i < 64; i++ {
		ranges = append(ranges, Range{i * mib, i*mib + 100<<10})
	}
	bs.FillBatch(context.Background(), k, ranges, size)
	if peak := bs.FillInflightPeak(); peak < 48 {
		t.Fatalf("fill inflight peak = %d, want ≥ ~64 (parallel dispatch of a 64-run batch)", peak)
	}
	if srv.GetCalls != 64 {
		t.Fatalf("GetCalls = %d, want 64 (one per run at this gap)", srv.GetCalls)
	}
}
