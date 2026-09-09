// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// siblingTestFS builds an FS over a directory of n small chunk objects
// (chunks/chunk.NNN, ~256 KiB each), with sibling readahead enabled and a
// prefetch policy of the given budget.
func siblingTestFS(t *testing.T, n int, budget int64) (*rawFS, *fake.Server, *SiblingStats, []uint64) {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	data := make([]byte, 256<<10) // 256 KiB, well under --small-file
	for i := range data {
		data[i] = byte(i)
	}
	for i := 0; i < n; i++ {
		srv.Put(fmt.Sprintf("chunks/chunk.%03d", i), data, now)
	}
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	bs, err := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 2 << 20, MemCache: 512 << 20, MaxRange: 8 << 20})
	if err != nil {
		t.Fatalf("blockstore: %v", err)
	}
	lim := prefetch.NewPolicy(budget, prefetch.DeviceLimits{}, func(key string, k int) []prefetch.Sibling {
		sibs := ix.Neighborhood(key, k)
		out := make([]prefetch.Sibling, len(sibs))
		for i, s := range sibs {
			out[i] = prefetch.Sibling{Key: s.Key, Size: s.Size, ETagHash: s.ETagHash}
		}
		return out
	})
	stats := &SiblingStats{}
	raw := NewRawFileSystem(Config{
		Index: ix, Store: bs, UID: 1000, GID: 1000,
		SmallFile: 4 << 20, PartsMax: 64 << 20,
		SiblingWindow: 4, SiblingReadahead: 16,
		Limits: lim, SiblingStats: stats,
	}).(*rawFS)

	// Resolve a node id for each chunk.
	nodes := make([]uint64, n)
	for i := 0; i < n; i++ {
		var eo1, eo2 fuse.EntryOut
		if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "chunks", &eo1); s != fuse.OK {
			t.Fatalf("lookup chunks: %v", s)
		}
		if s := raw.Lookup(nil, &fuse.InHeader{NodeId: eo1.NodeId}, fmt.Sprintf("chunk.%03d", i), &eo2); s != fuse.OK {
			t.Fatalf("lookup chunk %d: %v", i, s)
		}
		nodes[i] = eo2.NodeId
	}
	return raw, srv, stats, nodes
}

func openRead(t *testing.T, raw *rawFS, node uint64) {
	t.Helper()
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: node}}, &oo); s != fuse.OK {
		t.Fatalf("open: %v", s)
	}
	buf := make([]byte, 64<<10)
	if _, s := raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: node}, Fh: oo.Fh, Offset: 0, Size: 64 << 10}, buf); s != fuse.OK {
		t.Fatalf("read: %v", s)
	}
}

func settle(srv *fake.Server) { waitStableGets(srv) }

// TestSiblingReadaheadInOrder: opening chunk objects in key order triggers
// sibling readahead so later opens are prefetched (used), with few unread.
func TestSiblingReadaheadInOrder(t *testing.T) {
	raw, srv, stats, nodes := siblingTestFS(t, 20, 256<<20)
	for i := range nodes {
		openRead(t, raw, nodes[i])
		settle(srv)
	}
	settle(srv)

	if stats.Prefetched.Load() == 0 {
		t.Fatal("in-order walk prefetched no siblings")
	}
	// Most prefetched siblings should have been opened (used); the walk is
	// perfectly sequential so unread should be ~0 (only the tail beyond the last
	// open can be unread, and here readahead runs off the end).
	used := stats.Used.Load()
	if used < 10 {
		t.Fatalf("only %d sibling prefetches were used; expected the in-order walk to consume most", used)
	}
	// Every object was fetched at most once (coverage): total GETs ~= n objects.
	if got := srv.GetCallCount(); got > 20 {
		t.Fatalf("fetched objects in %d GETs, want <= 20 (one per object)", got)
	}
}

// TestSiblingReadaheadRandomOrder: opening in a scattered order (position gaps
// beyond the window) never detects a walk, so no sibling prefetch fires.
func TestSiblingReadaheadRandomOrder(t *testing.T) {
	raw, srv, stats, nodes := siblingTestFS(t, 20, 256<<20)
	// A scattered permutation with every step > sibling-window (4).
	order := []int{0, 10, 1, 15, 5, 19, 8, 2, 17, 6}
	for _, i := range order {
		openRead(t, raw, nodes[i])
		settle(srv)
	}
	settle(srv)
	if p := stats.Prefetched.Load(); p != 0 {
		t.Fatalf("scattered opens triggered %d sibling prefetches, want 0", p)
	}
}

// TestSiblingReadaheadBudgetExhausted: with a budget smaller than one sibling,
// sibling readahead reserves nothing and dispatches no prefetch, but reads still
// succeed (handle readahead is not starved).
func TestSiblingReadaheadBudgetExhausted(t *testing.T) {
	raw, srv, stats, nodes := siblingTestFS(t, 10, 64<<10) // 64 KiB budget < 256 KiB object
	for i := range nodes {
		openRead(t, raw, nodes[i]) // must not panic/error; reads served on demand
		settle(srv)
	}
	if p := stats.Prefetched.Load(); p != 0 {
		t.Fatalf("budget-exhausted sibling readahead prefetched %d siblings, want 0", p)
	}
}
