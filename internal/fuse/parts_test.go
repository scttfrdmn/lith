// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// partsTestFS builds an FS whose block (= part) size is 2 MiB, with the parts
// path enabled (PartsMax 64 MiB) and a budget policy wired in, over two objects:
// a 7 MiB file (4 parts) and a 1.5 MiB file (one part).
func partsTestFS(t *testing.T) (fuse.RawFileSystem, *fake.Server, uint64, uint64) {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	big := make([]byte, 7<<20)   // 7 MiB -> ceil(7/2) = 4 parts
	small := make([]byte, 3<<19) // 1.5 MiB -> 1 part
	for i := range big {
		big[i] = byte(i)
	}
	for i := range small {
		small[i] = byte(i * 3)
	}
	srv.Put("big.bin", big, now)
	srv.Put("small.bin", small, now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{
		Options: index.Options{Bucket: "bkt"},
	})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{
		Bucket: "bkt", BlockSize: 2 << 20, MemCache: 512 << 20, MaxRange: 8 << 20,
	})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(16<<20, prefetch.DeviceLimits{}, nil)
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, UID: 1000, GID: 1000,
		SmallFile: 512 << 10, PartsMax: 64 << 20, Limits: lim,
	})

	lookup := func(name string) uint64 {
		var eo fuse.EntryOut
		if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, name, &eo); s != fuse.OK {
			t.Fatalf("lookup %s: %v", name, s)
		}
		return eo.NodeId
	}
	return raw, srv, lookup("big.bin"), lookup("small.bin")
}

// triggerPartsFetch opens the node and issues a zero-length read at offset 0,
// which drives maybePartsFetch without a demand GET of its own, so the only
// GETs are the (idempotent, singleflight-coalesced) block prefetches.
func triggerPartsFetch(t *testing.T, raw fuse.RawFileSystem, node uint64) uint64 {
	t.Helper()
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: node}}, &oo); s != fuse.OK {
		t.Fatalf("open: %v", s)
	}
	if _, s := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: node}, Fh: oo.Fh, Offset: 0, Size: 0}, nil); s != fuse.OK {
		t.Fatalf("read: %v", s)
	}
	return oo.Fh
}

// waitStableGets polls until the fake's GET count has not changed for a short
// quiet window (the parts fetch runs in a background goroutine).
func waitStableGets(srv *fake.Server) int {
	last := -1
	stable := 0
	for i := 0; i < 400; i++ {
		n := srv.GetCallCount()
		if n == last {
			if stable++; stable >= 5 {
				return n
			}
		} else {
			stable = 0
			last = n
		}
		time.Sleep(2 * time.Millisecond)
	}
	return srv.GetCallCount()
}

// TestPartsFetchMultiPart: a 7 MiB file (block/part size 2 MiB) is fetched as
// multiple concurrent parts, and the whole file lands cached (a full read adds
// no further GETs).
func TestPartsFetchMultiPart(t *testing.T) {
	raw, srv, big, _ := partsTestFS(t)
	fh := triggerPartsFetch(t, raw, big)
	gets := waitStableGets(srv)

	// 7 MiB / 2 MiB parts = 4 concurrent block-sized range GETs, and no
	// open-time readahead races them (Open skips its window for parts-covered
	// files), so the count is exactly the part count.
	if gets != 4 {
		t.Fatalf("7 MiB file fetched in %d GETs, want 4 parallel parts", gets)
	}

	// The whole file is now cached: reading it end to end adds no GETs.
	buf := make([]byte, 1<<20)
	for off := int64(0); off < 7<<20; off += 1 << 20 {
		if _, s := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: big}, Fh: fh, Offset: uint64(off), Size: 1 << 20}, buf); s != fuse.OK {
			t.Fatalf("read at %d: %v", off, s)
		}
	}
	if after := srv.GetCallCount(); after != gets {
		t.Fatalf("full read after parts fetch added %d GETs (%d -> %d); whole file was not prefetched", after-gets, gets, after)
	}
}

// TestPartsFetchSinglePart: a file smaller than one part is fetched in a single
// GET.
func TestPartsFetchSinglePart(t *testing.T) {
	raw, srv, _, small := partsTestFS(t)
	triggerPartsFetch(t, raw, small)
	if gets := waitStableGets(srv); gets != 1 {
		t.Fatalf("1.5 MiB file (< one 2 MiB part) fetched in %d GETs, want 1", gets)
	}
}

// TestPartsFetchBudgetExhausted: when the prefetch budget cannot cover the file,
// the eager parts fetch is skipped (no reservation leak, no eager GETs beyond
// what a demand read needs).
func TestPartsFetchBudgetExhausted(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("big.bin", make([]byte, 7<<20), now)
	ix, _ := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 2 << 20, MemCache: 512 << 20, MaxRange: 8 << 20})
	lim := prefetch.NewPolicy(1<<20, prefetch.DeviceLimits{}, nil) // 1 MiB budget < 7 MiB file
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, UID: 1000, GID: 1000, PartsMax: 64 << 20, SmallFile: 512 << 10, Limits: lim})

	var eo fuse.EntryOut
	raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "big.bin", &eo)
	triggerPartsFetch(t, raw, eo.NodeId)
	gets := waitStableGets(srv)
	// The eager whole-file parts fetch is skipped (budget too small) and the
	// open-time window is not dispatched for a parts-covered file, so a
	// zero-length first read triggers no prefetch GETs at all.
	if gets != 0 {
		t.Fatalf("budget-exhausted parts fetch issued %d GETs; expected the eager fetch to be skipped", gets)
	}
	// The budget must be fully released (nothing leaked).
	if _, used := lim.Budget(); used != 0 {
		t.Fatalf("budget used = %d after skipped fetch, want 0", used)
	}
}
