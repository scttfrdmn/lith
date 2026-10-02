// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"testing"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// newWindowFS builds a rawFS whose budget is 492 blocks against a 223-block configured
// window: the shipping default's shape on a 33 GB box at 8 MiB blocks, which is the
// configuration the external #301 measurement was taken in.
func newWindowFS(t *testing.T) *rawFS {
	t.Helper()
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
	return f
}

// THE GUARD this function did not have (#301).
//
// perHandleWindow decides every handle's readahead depth, and the only reason its defect was
// ever found is that --pf-trace records peak_window while the mount logged the CONFIGURED
// value. Nothing asserted the realized depth at a divisor above one, so a function whose
// output falls by a factor of 111 across its range was covered by tests that all ran at the
// top of it. It was then changed, shipped, and reverted after an external 11x regression.
//
// So: assert the realized window across the range, against hand-computed values. These
// numbers are the division, written out.
func TestPerHandleWindowAcrossStreamCounts(t *testing.T) {
	f := newWindowFS(t)
	for _, tc := range []struct {
		streams int64
		want    int64
		why     string
	}{
		{0, 223, "no stream has established yet; the first reader gets everything"},
		{1, 223, "492/1 = 492, capped by --max-readahead"},
		{2, 223, "492/2 = 246, still above the cap"},
		{3, 164, "492/3 = 164, the first count the budget binds"},
		{8, 61, "492/8 = 61"},
		{16, 30, "492/16 = 30 — the external report's 16-reader cell"},
		{164, 3, "492/164 = 3, the last count above the floor"},
		{165, 2, "492/165 = 2 — THE FLOOR, one stream later"},
		{256, 2, "floored"},
		{10000, 2, "floored, far past it"},
	} {
		f.streamingHandles.Store(tc.streams)
		if got := f.perHandleWindow(); got != tc.want {
			t.Errorf("%d streams: window = %d, want %d (%s)", tc.streams, got, tc.want, tc.why)
		}
	}

	// The 111x collapse, reproduced: nothing the operator configured changes between these.
	f.streamingHandles.Store(1)
	alone := f.perHandleWindow()
	f.streamingHandles.Store(256)
	crowded := f.perHandleWindow()
	if alone/crowded < 100 {
		t.Errorf("window at 1 stream = %d, at 256 = %d (%.0fx): the collapse this guards "+
			"against is not reproduced, so the fixture no longer exercises it",
			alone, crowded, float64(alone)/float64(crowded))
	}
}

// THE FIX, asserted directly (#301): open descriptors do not shrink anyone's window.
//
// This is the defect the external workload paid 6-10x for. Both its production mounts sat at
// the 2-block floor because ~288 descriptors were open across 48 ranks, most of them never
// read, and every one of them was a divisor. An earlier version of this very test populated
// f.handles and expected the window to collapse; that it now fails is the change.
func TestOpenDescriptorsDoNotShrinkTheWindow(t *testing.T) {
	f := newWindowFS(t)
	// One reader, streaming. 287 other descriptors open and idle — the shape measured.
	f.streamingHandles.Store(1)
	for i := 1; i <= 288; i++ {
		f.handles[uint64(i)] = &fileHandle{}
	}
	if got := f.perHandleWindow(); got != 223 {
		t.Errorf("1 stream behind 288 open descriptors: window = %d, want 223 — the "+
			"descriptor count is back in the divisor", got)
	}
	if got := f.OpenHandles(); got != 288 {
		t.Errorf("OpenHandles = %d, want 288: the count must stay OBSERVABLE even though "+
			"it no longer sizes anything", got)
	}
	if got := f.StreamingHandles(); got != 1 {
		t.Errorf("StreamingHandles = %d, want 1", got)
	}
}

// No budget configured must not collapse the window: the division is skipped entirely, so
// many streams still each get the configured depth.
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
	f.streamingHandles.Store(256)
	if got := f.perHandleWindow(); got != 223 {
		t.Errorf("256 streams, no budget: window = %d, want 223 (no division applies)", got)
	}
}
