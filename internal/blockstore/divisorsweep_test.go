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

// PHASE 1 of #301: does the prefetch-budget divisor earn its cost in the regime it exists
// for?
//
// perHandleWindow() rations windowed readahead as clamp(budgetBlocks/openHandles, 2,
// maxReadahead). Measured on a real mount, that over-charges by exactly the handle count:
// committed bytes track ONE window however many handles are charged, so tightness = 1/N
// and a mount at 256 descriptors is charged 4295 MB while holding 17.8 MB — throttled
// 6–10x for it (#298, #301).
//
// But every one of those cells was a SINGLE streaming reader, so #55's thrash shape — many
// concurrent readers over a working set larger than the memory tier — was not represented,
// and the data cannot say whether the divisor's protection is needed. That is what this
// measures, in the fixture #55 itself was fixed against.
//
// THE ASSERTION IS THE EXPERIMENT, and it is a paired CONTRAST rather than a threshold: the
// `neutral` arm removes the divisor by letting every reader prefetch the whole budget ahead,
// and the divisor arm must do no worse on either of #55's signals. A first version borrowed
// TestPrefetchBudgetNoThrash's absolute 1% bound and failed in both arms, which is what
// surfaced the limitation below.
//
// WHAT THIS CANNOT SETTLE, and the reason it does not decide #301 on its own. The fake
// server returns instantly, so every dispatched block lands before consumption drains any
// of it. Peak committed therefore sits ABOVE the budget here where production sits well
// below it:
//
//	                committed as % of budget    % of tier
//	production N=1          45.3                  22.7
//	production N=16          6.1                   3.1
//	production N=256         0.4                   0.2
//	fixture    N=4         109.2                  54.6
//	fixture    N=16        142.1                  71.0
//
// (production figures measured on a real mount by the reporting workload, #301.)
//
// So this fixture runs 2.4-3.5x tighter on committed/tier than the default configuration
// does, in a direction the network cannot produce. It establishes that the divisor CAN
// protect #55's bounds under memory pressure; it does not establish that it does so at the
// shipping ratio, because the pressure is not the shipping pressure. That distinction is
// Phase 1b's job and it needs a real endpoint.
func TestPrefetchDivisorEarnsItsCost(t *testing.T) {
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

	// THE ASSERTION IS THE EXPERIMENT, and it is a CONTRAST rather than a threshold: an
	// absolute bound borrowed from TestPrefetchBudgetNoThrash's sizing does not transfer to
	// this fixture, as a first run showed by failing in both arms. What transfers is the
	// paired comparison, one variable apart.
	//
	// If the divisor arm is not materially better than neutral, the divisor is not what
	// protects #55 and Phase 2B is indicated. If it is, Phase 2A is.
	byCell := map[string]result{}
	for _, r := range table {
		byCell[r.ratio+"/"+r.arm+"/"+strconv.Itoa(r.readers)] = r
	}
	for _, ratio := range ratios {
		for _, readers := range []int{4, 8, 16} {
			d := byCell[ratio.name+"/divisor/"+strconv.Itoa(readers)]
			n := byCell[ratio.name+"/neutral/"+strconv.Itoa(readers)]
			if d.issued == 0 || n.issued == 0 {
				t.Fatalf("%s readers=%d: missing a cell", ratio.name, readers)
			}
			if d.evicted > n.evicted {
				t.Errorf("%s readers=%d: the divisor arm evicted MORE unread prefetch than the "+
					"neutral arm (%d vs %d) — the divisor is not protecting anything here",
					ratio.name, readers, d.evicted, n.evicted)
			}
			if d.s3Bytes > n.s3Bytes {
				t.Errorf("%s readers=%d: the divisor arm re-fetched more than neutral (%.3fx vs %.3fx)",
					ratio.name, readers, float64(d.s3Bytes)/float64(d.workingSet),
					float64(n.s3Bytes)/float64(n.workingSet))
			}
		}
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
