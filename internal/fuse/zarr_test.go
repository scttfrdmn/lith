// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	fzarr "github.com/scttfrdmn/lith/internal/format/zarr"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// TestPlanZarrChunks: the tier-1 fallback — prefetch the next chunks along the
// single walked axis, in grid order, and nothing else.
func TestPlanZarrChunks(t *testing.T) {
	grid := &fzarr.Grid{Dims: []int{3, 4, 5}} // 3×4×5

	if got := planZarrChunks("var", []int{0, 0, 1}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/0.0.2", "var/0.0.3", "var/0.0.4"}) {
		t.Fatalf("axis2 = %v", got)
	}
	if got := planZarrChunks("var", []int{1, 0, 0}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/2.0.0"}) {
		t.Fatalf("axis0 = %v", got)
	}
	if got := planZarrChunks("var", []int{0, 1, 0}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/0.2.0", "var/0.3.0"}) {
		t.Fatalf("axis1 = %v", got)
	}
	if got := planZarrChunks("var", []int{0, 0, 1}, []int{0, 0, 0}, grid, 1); !reflect.DeepEqual(got, []string{"var/0.0.2"}) {
		t.Fatalf("n=1 = %v", got)
	}
	// Ambiguous / edge -> nothing.
	if got := planZarrChunks("var", []int{0, 0, 1}, nil, grid, 8); got != nil {
		t.Fatalf("no-last = %v, want nil", got)
	}
	if got := planZarrChunks("var", []int{1, 1, 0}, []int{0, 0, 0}, grid, 8); got != nil {
		t.Fatalf("two-axes = %v, want nil", got)
	}
	if got := planZarrChunks("var", []int{0, 0, 4}, []int{0, 0, 3}, grid, 8); got != nil {
		t.Fatalf("edge = %v, want nil", got)
	}
}

// TestGridForZarray: per-array `.zarray` detection (no consolidated metadata).
func TestGridForZarray(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString("v/.zarray", `{"shape":[20,20,30],"chunks":[10,10,10]}`, now) // grid 2×2×3
	srv.Put("v/0.0.0", make([]byte, 4096), now)
	srv.PutString("bad/.zarray", `{"shape":[10]}`, now) // malformed
	srv.Put("bad/0.0.0", make([]byte, 4096), now)
	srv.PutString("other/file.txt", "hi", now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 4 << 20, MemCache: 64 << 20, MaxRange: 8 << 20})
	f := NewRawFileSystem(Config{Index: ix, Store: bs, SiblingReadahead: 16}).(*rawFS)

	if g := f.gridFor("v"); g == nil || !reflect.DeepEqual(g.Dims, []int{2, 2, 3}) {
		t.Fatalf("gridFor(v) = %v, want dims [2 2 3]", g)
	}
	if g := f.gridFor("bad"); g != nil {
		t.Fatalf("gridFor(bad) = %v, want nil (malformed .zarray)", g)
	}
	if g := f.gridFor("other"); g != nil {
		t.Fatalf("gridFor(other) = %v, want nil (no .zarray)", g)
	}
}

// openPath resolves a "/"-separated relative path via successive Lookups from
// the root and Opens the leaf (driving the readahead path).
func openPath(t *testing.T, raw *rawFS, rel string) {
	t.Helper()
	node := uint64(fuse.FUSE_ROOT_ID)
	for _, comp := range strings.Split(rel, "/") {
		var eo fuse.EntryOut
		if s := raw.Lookup(nil, &fuse.InHeader{NodeId: node}, comp, &eo); s != fuse.OK {
			t.Fatalf("lookup %q in %s: %v", comp, rel, s)
		}
		node = eo.NodeId
	}
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: node}}, &oo); s != fuse.OK {
		t.Fatalf("open %s: %v", rel, s)
	}
}

// TestZarrTier2PlaneAndConsolidated: one consolidated `.zmetadata` populates the
// grids of multiple arrays (no per-array `.zarray` object needed), and a walk
// that turns a corner re-plans to the 2-D plane — prefetching chunks that differ
// from every open on two axes, which tier-1 (single-axis line) never would.
func TestZarrTier2PlaneAndConsolidated(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString(".zmetadata", `{"zarr_consolidated_format":1,"metadata":{`+
		`"sf/.zarray":{"shape":[2,4,5],"chunks":[1,1,1],"dtype":"<i4"},`+
		`"time/.zarray":{"shape":[5],"chunks":[1],"dtype":"<i8"},`+
		`".zgroup":{"zarr_format":2}}}`, now)
	for i := 0; i < 2; i++ {
		for j := 0; j < 4; j++ {
			for k := 0; k < 5; k++ {
				srv.Put(fmt.Sprintf("sf/%d.%d.%d", i, j, k), make([]byte, 4096), now)
			}
		}
	}
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 2 << 20, MemCache: 128 << 20, MaxRange: 8 << 20})
	pol := prefetch.NewPolicy(64<<20, prefetch.DeviceLimits{}, func(key string, n int) []prefetch.Sibling {
		out := []prefetch.Sibling{}
		for _, s := range ix.Neighborhood(key, n) {
			out = append(out, prefetch.Sibling{Key: s.Key, Size: s.Size, ETagHash: s.ETagHash})
		}
		return out
	})
	stats := &SiblingStats{}
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, SiblingReadahead: 16, SmallFile: 1 << 20, PartsMax: 4 << 20, Limits: pol, SiblingStats: stats}).(*rawFS)

	// Consolidated: both arrays' grids come from the single `.zmetadata` — note
	// there is no `time/.zarray` object nor any `time/*` chunk in the store.
	if g := raw.gridFor("sf"); g == nil || !reflect.DeepEqual(g.Dims, []int{2, 4, 5}) {
		t.Fatalf("gridFor(sf) = %v, want dims [2 4 5]", g)
	}
	if g := raw.gridFor("time"); g == nil || !reflect.DeepEqual(g.Dims, []int{5}) {
		t.Fatalf("gridFor(time) = %v, want dims [5] from .zmetadata", g)
	}

	// Walk axis 1, then turn the corner onto axis 2 → re-plan to the 4×5 plane.
	for _, name := range []string{"sf/0.0.0", "sf/0.1.0", "sf/0.2.0", "sf/0.0.1"} {
		openPath(t, raw, name)
	}
	waitStableGets(srv)

	// The 2-D plane (axes 1,2 varying, axis 0 fixed at 0) must have issued chunks
	// that differ on *two* axes from every open — e.g. 0.3.4 — which a single-axis
	// tier-1 line can never reach.
	raw.zarr.mu.Lock()
	issued := map[string]bool{}
	for k := range raw.zarr.issued["sf"] {
		issued[k] = true
	}
	raw.zarr.mu.Unlock()
	if !issued["0.3.4"] {
		t.Fatalf("plane chunk 0.3.4 not issued — tier-2 plane not active; issued=%v", issued)
	}
	if n := stats.Prefetched.Load(); n < 12 {
		t.Fatalf("prefetched=%d, want ≈ the 4×5 plane (>=12)", n)
	}
}

// TestZarrMalformedConsolidatedFallsToZarray: a malformed `.zmetadata` does not
// break detection or panic — a valid per-array `.zarray` still yields a grid.
func TestZarrMalformedConsolidatedFallsToZarray(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.PutString(".zmetadata", `{"metadata":{"sf/.zarray":{"shape":[oops`, now) // truncated/garbage
	srv.PutString("sf/.zarray", `{"shape":[6,6],"chunks":[2,2]}`, now)           // grid 3×3
	srv.Put("sf/0.0", make([]byte, 4096), now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 4 << 20, MemCache: 64 << 20, MaxRange: 8 << 20})
	f := NewRawFileSystem(Config{Index: ix, Store: bs, SiblingReadahead: 16}).(*rawFS)

	if g := f.gridFor("sf"); g == nil || !reflect.DeepEqual(g.Dims, []int{3, 3}) {
		t.Fatalf("gridFor(sf) = %v, want dims [3 3] via .zarray fallback", g)
	}
}
