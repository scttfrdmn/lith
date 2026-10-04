// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// newColdFS builds a mount at the SHIPPING block size of 8 MiB.
//
// newByteExactFS uses 1 MiB blocks, which makes one chunk per block and hides every effect
// that depends on a block spanning several chunks -- including the one below. A first version
// of this test reused it and concluded lith issued 8x the necessary GETs; that was the
// fixture, not lith.
func newColdFS(t *testing.T, objBytes int64) (*rawFS, *fake.Server) {
	t.Helper()
	srv := fake.New()
	srv.Put("big.bin", bytes.Repeat([]byte("A"), int(objBytes)), time.Unix(1_700_000_000, 0))
	ix, err := index.BuildFromList(context.Background(), srv,
		index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 8 << 20, MemCache: 512 << 20, MaxRange: 64 << 20,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(256<<20,
		prefetch.DeviceLimits{NICBytesPerSec: 100 << 20, TTFB: 10 * time.Millisecond}, nil)
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, Metrics: metrics.New(), UID: 1000, GID: 1000,
		Limits: lim, MaxReadahead: 32,
	}).(*rawFS)
	return raw, srv
}

// #233, reproduced offline: a cold sequential reader pays block 0 as separate chunk GETs.
//
// lith pins FUSE MaxWrite at 128 KiB (deliberately; see internal/fuse/mount.go), so a
// sequential reader's reads stay inside one 8 MiB block for 64 of them -- the detector sees
// d == 0 and returns without a state change. Establishment (#229) needs a BLOCK ADVANCE, so
// it cannot fire until the reader crosses into block 1, and block 0 is therefore demand-served
// as eight 1 MiB chunks instead of one coalesced 8 MiB fetch.
//
// #233 measured that on real hardware (GET 4 -> 12, wall +55% on a 30 MB `cat`) and recorded
// three refuted approaches, all of which break a witness: a copy's early 128 KiB reads into
// block 0 are indistinguishable from a scattered-but-locally-dense reader's. So this is a
// characterization, not a defect with a pending fix -- the test exists so the cost is pinned
// and the next person does not re-derive it.
//
// IT IS ALSO A NEGATIVE RESULT for #284. An external deployment fitted a per-open intercept of
// 0.47 s on a 1.5 GB/s box -- 72% of a 260 MB read's wall -- and we both suspected window
// establishment on each new handle. The numbers below rule that out: 22 GETs for 64 MiB is
// ~48 ms even fully serialized at their measured 2.2 ms, two orders off 470 ms. Whatever the
// intercept is, it is not the GET count and not establishment.
func TestColdSequentialGetShape(t *testing.T) {
	const objBytes = int64(64) << 20
	const readLen = int64(128) << 10 // what the kernel actually hands FUSE
	const blockSize = int64(8) << 20
	raw, srv := newColdFS(t, objBytes)
	node, fh := openBig(t, raw)
	h := raw.handleOf(fh)

	estAt, block0Gets := -1, 0
	n := int(objBytes / readLen)
	prev := srv.GetCallCount()
	for i := 0; i < n; i++ {
		off := int64(i) * readLen
		readAt(t, raw, node, fh, off, readLen)
		g := srv.GetCallCount()
		if off < blockSize {
			block0Gets += g - prev
		}
		prev = g
		if estAt < 0 && h.pf.state() == prefetch.Sequential {
			estAt = i
		}
	}
	total := quiesce(srv)
	gets := srv.GetCallCount()

	t.Logf("object %d MiB, %d reads of %d KiB, %d reads per %d MiB block",
		objBytes>>20, n, readLen>>10, blockSize/readLen, blockSize>>20)
	t.Logf("established Sequential at read %d = byte %d = block %d",
		estAt, int64(estAt)*readLen, int64(estAt)*readLen/blockSize)
	t.Logf("GETs %d for %d MiB  (one per block would be %d, one per chunk %d)",
		gets, total>>20, objBytes/blockSize, objBytes/int64(blockstore.ChunkSize))
	t.Logf("GETs inside BLOCK 0, before any block crossing: %d", block0Gets)

	// Establishment cannot happen before the first block crossing, which is what makes
	// block 0 expensive. If this moves earlier, #233's premise has changed.
	wantEst := int(blockSize / readLen)
	if estAt < wantEst {
		t.Errorf("established at read %d, before the first block crossing at %d — #233's "+
			"mechanism no longer holds, and its three refuted approaches may be worth "+
			"revisiting", estAt, wantEst)
	}

	// Block 0 costs one GET per chunk. This is the number #233 is about.
	if want := int(blockSize / int64(blockstore.ChunkSize)); block0Gets != want {
		t.Errorf("block 0 cost %d GETs, want %d (one per 1 MiB chunk) — the cold-start "+
			"coalescing behaviour has changed", block0Gets, want)
	}

	// THE FIGURE, pinned. 22 before #332 and 17 after, both stable across six runs (the
	// post-fix value occasionally reads 18, so the bound is 20). The earlier bound here was
	// 4x the ideal — 32 — which would not have noticed #332 being reverted, and a fix worth
	// ~140 ms per open should not rest on a bound that loose.
	//
	// 22 -> 17 is block 1's five GETs. Block 0's eight remain and are #233.
	if gets > 20 {
		t.Errorf("%d GETs for %d MiB, more than 20 — this was 22 before #332 and 17 after, so "+
			"either the establishment dispatch no longer covers the block the reader is in, "+
			"or fetching has degenerated toward per-chunk", gets, objBytes>>20)
	}
	if gets < int(objBytes/blockSize) {
		t.Errorf("%d GETs for %d MiB is below one per block (%d): the fixture is serving from "+
			"cache and measures nothing", gets, objBytes>>20, objBytes/blockSize)
	}
	if total != objBytes {
		t.Errorf("fetched %d bytes for a %d-byte object: a whole sequential read must be "+
			"byte-exact", total, objBytes)
	}
}
