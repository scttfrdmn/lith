// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// #301: committed prefetch bytes must never exceed --prefetch-budget, however aggressively
// the caller dispatches.
//
// This test began as "does the divisor earn its cost?" and that question is answered, in
// bench/prefetch-divisor: rationing is essential (removing it costs 13.8-22.4x wall and
// 4.2-5.2x the bytes with 81-92% of prefetch evicted unread) but the divisor rationed by
// DESCRIPTOR COUNT, which over-charges by exactly that count. admitCommitted now enforces the
// budget byte-exactly where it is measured, so the proxy is gone.
//
// What the two arms are for now: they dispatch very differently -- `divisor` advances a
// frontier budgetBlocks/N ahead as the old per-handle window did, `neutral` dispatches the
// whole budget ahead at once -- and the invariant must hold in both. That is the point of
// moving enforcement to the measured quantity: safety stops depending on the caller getting
// its own rationing right.
//
// The #55 signals (unread evictions, re-fetch) are REPORTED rather than asserted here,
// because this fixture runs at 2.4-3.5x production memory pressure: the fake server returns
// instantly, so dispatch outruns consumption in a way the network cannot, and peak committed
// sat at 109-142% of budget before admission existed where a real mount sits at 0.4-45%.
// TestPrefetchBudgetNoThrash is the #55 gate; this is the admission gate.
func TestPrefetchAdmissionCapsCommittedWhateverTheCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~0.5 GB of fixture objects")
	}
	const (
		chunksPerObj = 32             // 32 MiB objects
		blockSize    = int64(1) << 20 // 1 MiB blocks, so budgetBlocks has room to divide
		budget       = int64(48) << 20
	)
	// Two budget-to-tier ratios, because they are not the same question. "shipping" is the
	// default (--prefetch-budget is 50% of --mem-cache). "conservative" is the 1:8 that
	// TestPrefetchBudgetNoThrash deliberately chose, which is the only ratio #55's bounds
	// have ever been asserted at.
	ratios := []struct {
		name     string
		memCache int64
	}{
		{"shipping", 2 * budget},
		{"conservative", 8 * budget},
	}

	type result struct {
		readers     int
		ratio       string
		arm         string
		ahead       int64
		peakCommit  int64
		budgetBytes int64
		evicted     int64
		issued      int64
		s3Bytes     int64
		workingSet  int64
	}
	var table []result

	for _, ratio := range ratios {
		for _, readers := range []int{4, 8, 16} {
			for _, arm := range []string{"divisor", "neutral"} {
				srv := fake.New()
				keys := make([]Key, readers)
				for i := 0; i < readers; i++ {
					name := "obj" + strconv.Itoa(i)
					makeObj(srv, name, chunksPerObj)
					keys[i] = keyFor(t, srv, name)
				}
				rec := &budgetRec{}
				bs := newStore(t, srv, Config{
					BlockSize: blockSize, MemCache: ratio.memCache, PrefetchBudget: budget, Recorder: rec,
				})
				budgetBlocks := bs.PrefetchBudgetBlocks()

				// The only difference between the arms. `divisor` is what perHandleWindow does
				// today; `neutral` lets every handle prefetch the whole budget ahead, which is
				// what removing the divisor would permit.
				ahead := budgetBlocks
				if arm == "divisor" {
					if ahead = budgetBlocks / int64(readers); ahead < 2 {
						ahead = 2
					}
				}

				peak := sampleCommittedPeak(bs)
				runReaders(t, bs, keys, chunksPerObj, blockSize, ahead)
				table = append(table, result{
					readers: readers, ratio: ratio.name, arm: arm, ahead: ahead,
					peakCommit: peak(), budgetBytes: bs.PrefetchBudgetBytes(),
					evicted: rec.evicted.Load(), issued: rec.issued.Load(),
					s3Bytes:    rec.bytes.Load(),
					workingSet: int64(readers) * int64(chunksPerObj) * mib,
				})
			}
		}
	}

	t.Logf("%-13s %-8s %-8s %6s %12s %10s %9s %8s %10s", "ratio", "readers", "arm", "ahead",
		"peakCommit", "%ofBudget", "evicted", "issued", "s3/working")
	for _, r := range table {
		t.Logf("%-13s %-8d %-8s %6d %10.1f MB %9.1f%% %9d %8d %9.3fx",
			r.ratio, r.readers, r.arm, r.ahead, float64(r.peakCommit)/1e6,
			100*float64(r.peakCommit)/float64(r.budgetBytes),
			r.evicted, r.issued, float64(r.s3Bytes)/float64(r.workingSet))
	}

	// THE INVARIANT. Peak committed must not exceed the budget in any cell. Before
	// admitCommitted this reached 109-142% of budget in the divisor arm and up to 1067% in
	// the neutral arm, because the per-handle window was a proxy and a proxy can be wrong in
	// either direction. A byte-exact cap cannot be.
	for _, r := range table {
		if r.peakCommit > r.budgetBytes {
			t.Errorf("%s readers=%d arm=%s: peak committed %d exceeded the budget %d (%.1f%%) — "+
				"admitCommitted must cap it whatever the caller dispatches",
				r.ratio, r.readers, r.arm, r.peakCommit, r.budgetBytes,
				100*float64(r.peakCommit)/float64(r.budgetBytes))
		}
	}
	// And it must actually be reached, or the fixture has stopped exercising the cap and the
	// assertion above has gone vacuous.
	var anyAtCap bool
	for _, r := range table {
		if r.peakCommit*100 >= r.budgetBytes*90 {
			anyAtCap = true
		}
	}
	if !anyAtCap {
		t.Error("no cell drove committed within 10% of the budget: the fixture no longer " +
			"exercises admission, so the cap assertion proves nothing")
	}
}

// sampleCommittedPeak polls committed bytes until the returned func is called, which stops
// sampling and reports the peak. Polling is the only way to see a peak: committed bytes are
// a level, and the establishment burst that sets the level lands inside one sample interval.
func sampleCommittedPeak(bs *BlockStore) func() int64 {
	var peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(200 * time.Microsecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if v := bs.PrefetchCommittedBytes(); v > peak.Load() {
					peak.Store(v)
				}
			}
		}
	}()
	var once sync.Once
	return func() int64 {
		once.Do(func() { close(stop); <-done })
		return peak.Load()
	}
}

// runReaders is TestPrefetchBudgetNoThrash's reader loop, extracted so both tests drive the
// store identically: advance a prefetch frontier `ahead` blocks in front of the cursor,
// dispatching each block exactly once as the FUSE prefetcher does, then demand-read.
func runReaders(t *testing.T, bs *BlockStore, keys []Key, chunksPerObj int, blockSize, ahead int64) {
	t.Helper()
	blk := blockSize / ChunkSize
	if blk < 1 {
		blk = 1
	}
	nBlocks := int64(chunksPerObj) / blk
	objSize := int64(chunksPerObj) * mib
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func(k Key) {
			defer wg.Done()
			ctx := context.Background()
			frontier := int64(0)
			for b := int64(0); b < nBlocks; b++ {
				for target := b + ahead; frontier < target && frontier < nBlocks; frontier++ {
					go bs.Prefetch(ctx, k, frontier, objSize)
				}
				for ci := b * blk; ci < (b+1)*blk; ci++ {
					if _, err := bs.Chunk(ctx, k, ci, objSize, 0, ChunkSize, true); err != nil {
						t.Errorf("Chunk %d: %v", ci, err)
						return
					}
				}
			}
		}(k)
	}
	wg.Wait()
}
