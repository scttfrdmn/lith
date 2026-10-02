// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// THE GUARD this function did not have (#301).
//
// perHandleWindow decides every handle's readahead depth, and the only reason its defect was
// ever found is that --pf-trace records peak_window while the mount logged the CONFIGURED
// value. Nothing asserted the realized depth at a handle count above one, so a function whose
// output falls by a factor of 111 between N=1 and N=256 was covered by tests that all ran at
// N=1. It was then changed, shipped, and reverted after an external 11x regression.
//
// So: assert the realized window across the range, against hand-computed values, with the
// handle map populated directly. These numbers are the division, written out.
func TestPerHandleWindowAcrossHandleCounts(t *testing.T) {
	// 492 blocks of budget against a 223-block configured window: the shipping default's
	// shape on a 33 GB box at 8 MiB blocks, which is the configuration the external
	// measurement was taken in.
	const blockSize, maxRA = 1 << 20, int64(223)
	srv := fake.New()
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: blockSize, MemCache: 1 << 20, MaxRange: 1 << 20,
		PrefetchBudget: 492 * blockSize,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	f := &rawFS{
		cfg:     Config{Store: bs, MaxReadahead: maxRA},
		store:   bs,
		handles: map[uint64]*fileHandle{},
	}
	if got := f.budgetBlocks(); got != 492 {
		t.Fatalf("fixture: budgetBlocks = %d, want 492", got)
	}

	for _, tc := range []struct {
		handles int
		want    int64
		why     string
	}{
		{1, 223, "492/1 = 492, capped by --max-readahead"},
		{2, 223, "492/2 = 246, still above the cap"},
		{3, 164, "492/3 = 164, the first count the budget binds"},
		{8, 61, "492/8 = 61"},
		{16, 30, "492/16 = 30 — the external report's 16-handle cell"},
		{164, 3, "492/164 = 3, the last count above the floor"},
		{165, 2, "492/165 = 2 — THE FLOOR, one descriptor later"},
		{256, 2, "floored; the external report's 256-handle cell"},
		{10000, 2, "floored, far past it"},
	} {
		// Populate the map to exactly this size. perHandleWindow reads only len().
		for len(f.handles) < tc.handles {
			f.handles[uint64(len(f.handles))+1] = &fileHandle{}
		}
		for len(f.handles) > tc.handles {
			for k := range f.handles {
				delete(f.handles, k)
				break
			}
		}
		if got := f.perHandleWindow(); got != tc.want {
			t.Errorf("%d handles: window = %d, want %d (%s)", tc.handles, got, tc.want, tc.why)
		}
	}

	// The 111x collapse the external workload measured, reproduced locally: nothing the
	// operator configured changed between these two, only how many descriptors were open.
	f.handles = map[uint64]*fileHandle{}
	f.handles[1] = &fileHandle{}
	alone := f.perHandleWindow()
	for i := 2; i <= 256; i++ {
		f.handles[uint64(i)] = &fileHandle{}
	}
	crowded := f.perHandleWindow()
	if alone/crowded < 100 {
		t.Errorf("window at 1 handle = %d, at 256 = %d (%.0fx): the collapse this guards "+
			"against is not reproduced, so the fixture no longer exercises it",
			alone, crowded, float64(alone)/float64(crowded))
	}
}

// No budget configured must not collapse the window: the division is skipped entirely, so
// 256 handles still get the configured depth.
//
// Reached through a zero-cap Limits policy rather than a bare store, because a store always
// derives a default budget -- the first version of this test routed through the store and
// SKIPPED, which asserts nothing while looking like coverage.
func TestPerHandleWindowWithoutABudget(t *testing.T) {
	srv := fake.New()
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 1 << 20, MemCache: 1 << 20, MaxRange: 1 << 20,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	f := &rawFS{
		cfg: Config{
			Store:        bs,
			MaxReadahead: 223,
			Limits:       prefetch.NewPolicy(0, prefetch.DeviceLimits{}, nil),
		},
		store:   bs,
		handles: map[uint64]*fileHandle{},
	}
	if got := f.budgetBlocks(); got != 0 {
		t.Fatalf("fixture: budgetBlocks = %d, want 0 — the no-budget path is not being exercised", got)
	}
	for i := 1; i <= 256; i++ {
		f.handles[uint64(i)] = &fileHandle{}
	}
	if got := f.perHandleWindow(); got != 223 {
		t.Errorf("256 handles, no budget: window = %d, want 223 (no division applies)", got)
	}
}
