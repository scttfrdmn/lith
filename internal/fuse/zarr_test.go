// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func TestChunkCoords(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
		ok   bool
	}{
		{"0.0.5", []int{0, 0, 5}, true},
		{"12.3", []int{12, 3}, true},
		{"7", []int{7}, true},
		{".zarray", nil, false},
		{".zattrs", nil, false},
		{"", nil, false},
		{"0.x.1", nil, false},
	} {
		got, ok := chunkCoords(tc.in)
		if ok != tc.ok || (ok && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("chunkCoords(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseZarray(t *testing.T) {
	// shape 3000×4000×5000, chunks 1000×1000×1000 -> grid 3×4×5.
	g, ok := parseZarray([]byte(`{"shape":[3000,4000,5000],"chunks":[1000,1000,1000],"dtype":"<f8"}`))
	if !ok || !reflect.DeepEqual(g.dims, []int{3, 4, 5}) {
		t.Fatalf("grid = %v,%v want dims [3 4 5]", g, ok)
	}
	// ceil: shape 10, chunk 3 -> 4.
	if g, ok := parseZarray([]byte(`{"shape":[10],"chunks":[3]}`)); !ok || g.dims[0] != 4 {
		t.Fatalf("ceil grid = %v,%v want [4]", g, ok)
	}
	// Malformed / mismatched -> not ok (silent fallback).
	for _, bad := range []string{`not json`, `{"shape":[10]}`, `{"shape":[10],"chunks":[0]}`, `{"shape":[],"chunks":[]}`} {
		if _, ok := parseZarray([]byte(bad)); ok {
			t.Errorf("parseZarray(%q) unexpectedly ok", bad)
		}
	}
}

// TestPlanZarrChunks: the core #70 behavior — prefetch the next chunks along the
// axis the app is walking, in grid order, and nothing else.
func TestPlanZarrChunks(t *testing.T) {
	grid := &zarrGrid{dims: []int{3, 4, 5}} // 3×4×5

	// Walk axis 2 (last dim): 0.0.0 -> 0.0.1 prefetches 0.0.2,0.0.3,0.0.4 (clamped at 5).
	if got := planZarrChunks("var", []int{0, 0, 1}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/0.0.2", "var/0.0.3", "var/0.0.4"}) {
		t.Fatalf("axis2 = %v", got)
	}
	// Walk axis 0 (first dim): 0.0.0 -> 1.0.0 prefetches 2.0.0 (clamped at 3), NOT any axis-2 key.
	if got := planZarrChunks("var", []int{1, 0, 0}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/2.0.0"}) {
		t.Fatalf("axis0 = %v", got)
	}
	// Walk axis 1: 0.1.0 after 0.0.0 -> 0.2.0, 0.3.0.
	if got := planZarrChunks("var", []int{0, 1, 0}, []int{0, 0, 0}, grid, 8); !reflect.DeepEqual(got, []string{"var/0.2.0", "var/0.3.0"}) {
		t.Fatalf("axis1 = %v", got)
	}
	// n limits the count.
	if got := planZarrChunks("var", []int{0, 0, 1}, []int{0, 0, 0}, grid, 1); !reflect.DeepEqual(got, []string{"var/0.0.2"}) {
		t.Fatalf("n=1 = %v", got)
	}
	// Ambiguous: no previous open, backward step, or two axes changed -> nothing.
	if got := planZarrChunks("var", []int{0, 0, 1}, nil, grid, 8); got != nil {
		t.Fatalf("no-last = %v, want nil", got)
	}
	if got := planZarrChunks("var", []int{0, 0, 0}, []int{0, 0, 1}, grid, 8); got != nil {
		t.Fatalf("backward = %v, want nil", got)
	}
	if got := planZarrChunks("var", []int{1, 1, 0}, []int{0, 0, 0}, grid, 8); got != nil {
		t.Fatalf("two-axes = %v, want nil", got)
	}
	// Already at the grid edge along the walked axis -> nothing to prefetch.
	if got := planZarrChunks("var", []int{0, 0, 4}, []int{0, 0, 3}, grid, 8); got != nil {
		t.Fatalf("edge = %v, want nil", got)
	}
}

// TestGridForDetection: detection is Index-based (.zarray sibling) + a one-time
// parse; a valid store yields a grid, a non-zarr dir or a malformed .zarray
// yields nil (silent key-order fallback).
func TestGridForDetection(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	// valid zarr variable "v": grid 2×2×3
	srv.PutString("v/.zarray", `{"shape":[20,20,30],"chunks":[10,10,10]}`, now)
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			for k := 0; k < 3; k++ {
				srv.Put(fmt.Sprintf("v/%d.%d.%d", i, j, k), make([]byte, 4096), now)
			}
		}
	}
	// malformed .zarray in "bad"; non-zarr file in "other"
	srv.PutString("bad/.zarray", `{"shape":[10]}`, now) // shape without chunks
	srv.Put("bad/0.0.0", make([]byte, 4096), now)
	srv.PutString("other/file.txt", "hi", now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "bkt", BlockSize: 4 << 20, MemCache: 64 << 20, MaxRange: 8 << 20})
	f := NewRawFileSystem(Config{Index: ix, Store: bs, SiblingReadahead: 16}).(*rawFS)

	if g := f.gridFor("v"); g == nil || !reflect.DeepEqual(g.dims, []int{2, 2, 3}) {
		t.Fatalf("gridFor(v) = %v, want dims [2 2 3]", g)
	}
	if g := f.gridFor("bad"); g != nil {
		t.Fatalf("gridFor(bad) = %v, want nil (malformed .zarray -> fallback)", g)
	}
	if g := f.gridFor("other"); g != nil {
		t.Fatalf("gridFor(other) = %v, want nil (no .zarray)", g)
	}
	// cached: second call for a non-zarr dir stays nil without re-checking.
	if g := f.gridFor("other"); g != nil {
		t.Fatalf("gridFor(other) cached = %v, want nil", g)
	}
}
