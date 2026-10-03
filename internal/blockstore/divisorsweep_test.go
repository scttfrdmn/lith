// SPDX-License-Identifier: Apache-2.0

package blockstore

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// #301: does the prefetch-budget divisor earn its cost?
//
// perHandleWindow rations readahead as clamp(budgetBlocks/N, 2, maxReadahead). Replacing the
// division with byte-exact admission on the same TOTAL was tried and REVERTED (see the divisor
// comment in internal/fuse/fs.go): it starved concurrent readers by up to 11x, because the
// division is an ALLOCATION DISCIPLINE and not merely a total. 16 x 30 blocks covers sixteen
// readers shallowly; 2 x 223 + 14 x 0 commits the same total and covers two.
//
// N is the count of established sequential streams (#301; it was the open-descriptor count,
// which over-charged by exactly that count). Every reader in this fixture streams, so N here
// is the reader count either way -- which is why this sweep is unchanged by that fix and
// equally cannot measure it. What the input change buys is depth for mounts with idle
// descriptors, and depth costs WALL CLOCK, which a fake server returning instantly cannot
// price at all. That half needs real S3.
//
// So this guards the share. The two arms are one variable apart: `divisor` advances a
// frontier budgetBlocks/N ahead as perHandleWindow does, `neutral` advances the whole budget
// ahead as removing the division would permit. The divisor arm must do no worse on either of
// #55's signals -- which is a CONTRAST, not a threshold: an absolute bound borrowed from
// TestPrefetchBudgetNoThrash's sizing does not transfer here, as a first version showed by
// failing in both arms.
//
// WHAT THIS CANNOT SETTLE. The fake server returns instantly, so dispatch outruns consumption
// in a way the network cannot, and peak committed sits ABOVE the budget here where a real
// mount sits at 0.4-45% of it. bench/prefetch-divisor carries the real-S3 numbers and is what
// should be quoted; this is a cheap guard on the direction.
func TestPrefetchDivisorEarnsItsCost(t *testing.T) {
	// Two sizes. CI runs the small one: enough readers and oversubscription to exercise
	// admission and prove the invariant, small enough to survive `go test -race`, which
	// OOM-killed a GitHub runner at the measurement size (exit 143) because the race
	// detector's shadow memory multiplies a 512 MB fixture.
	//
	// LITH_SWEEP_FULL=1 runs the measurement size, which is what the numbers in
	// bench/prefetch-divisor were taken at. The invariant is the same either way; only the
	// reported magnitudes need the larger fixture.
	// 8 chunks x 4 readers = 32 MiB of distinct demand against a 32 MiB budget, so the
	// largest cell drives committed to the cap -- which the vacuity check below requires,
	// and which a smaller fixture failed to do. The budget also sets the tiers (2x and 8x),
	// and it is deliberately NOT shrunk further even though it now could be: a --mem-cache
	// under 64 MiB used to be dead, because 64 fixed shards of under 1 MiB each refused every
	// chunk, and that constraint is gone since newMemCache scales the shard count (#307).
	// The fixture stays where the published numbers were taken, because changing it would
	// change what bench/prefetch-divisor's figures mean.
	chunksPerObj, readerCounts, budget := 8, []int{2, 4}, int64(32)<<20
	if os.Getenv("LITH_SWEEP_FULL") != "" {
		chunksPerObj, readerCounts, budget = 32, []int{4, 8, 16}, int64(48)<<20
	}
	const blockSize = int64(1) << 20 // 1 MiB blocks, so budgetBlocks has room to divide
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
		for _, readers := range readerCounts {
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

	// A loose sanity bound on the DIVISOR arm at any fixture size: the shipping
	// configuration may not evict most of what it prefetched or re-fetch half the working
	// set. Catches a catastrophe in CI without pretending the small fixture supports a finer
	// claim.
	//
	// Scoped to the divisor arm deliberately. `neutral` is the counterfactual and is
	// expected to be bad -- at the measurement size it evicts 135 of 256 and fetches 1.527x,
	// which is the finding rather than a regression. Asserting the bound on both arms flagged
	// it as a failure, which is the mirror of the vacuous assertion: a true measurement
	// reported as a defect.
	for _, r := range table {
		if r.arm != "divisor" {
			continue
		}
		if r.issued > 0 && r.evicted*2 > r.issued {
			t.Errorf("%s readers=%d arm=%s: evicted %d of %d issued — over half the prefetch was "+
				"thrown away unread", r.ratio, r.readers, r.arm, r.evicted, r.issued)
		}
		if r.s3Bytes > r.workingSet*3/2 {
			t.Errorf("%s readers=%d arm=%s: fetched %.3fx the working set",
				r.ratio, r.readers, r.arm, float64(r.s3Bytes)/float64(r.workingSet))
		}
	}

	// THE CONTRAST needs the measurement fixture and is skipped without it. At the CI size
	// the objects are 8 blocks, so `ahead` covers the whole object in BOTH arms and the
	// difference is noise -- asserting it there failed, which is the vacuous-assertion trap
	// in its other form: a claim the fixture cannot produce. bench/prefetch-divisor carries
	// the real-S3 version, and that is what should be quoted.
	if os.Getenv("LITH_SWEEP_FULL") == "" {
		return
	}
	byCell := map[string]result{}
	for _, r := range table {
		byCell[r.ratio+"/"+r.arm+"/"+strconv.Itoa(r.readers)] = r
	}
	for _, ratio := range ratios {
		for _, readers := range readerCounts {
			d := byCell[ratio.name+"/divisor/"+strconv.Itoa(readers)]
			n := byCell[ratio.name+"/neutral/"+strconv.Itoa(readers)]
			if d.issued == 0 || n.issued == 0 {
				t.Fatalf("%s readers=%d: missing a cell", ratio.name, readers)
			}
			if d.evicted > n.evicted {
				t.Errorf("%s readers=%d: the divisor arm evicted MORE unread prefetch than neutral "+
					"(%d vs %d) — the share has stopped protecting anything",
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
