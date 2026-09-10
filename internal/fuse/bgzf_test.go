// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/prefetch"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

func openHandle(t *testing.T, raw *rawFS, rel string) (*fileHandle, uint64) {
	t.Helper()
	var eo fuse.EntryOut
	if s := raw.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, rel, &eo); s != fuse.OK {
		t.Fatalf("lookup %s: %v", rel, s)
	}
	var oo fuse.OpenOut
	if s := raw.Open(nil, &fuse.OpenIn{InHeader: fuse.InHeader{NodeId: eo.NodeId}}, &oo); s != fuse.OK {
		t.Fatalf("open %s: %v", rel, s)
	}
	raw.mu.RLock()
	h := raw.handles[oo.Fh]
	raw.mu.RUnlock()
	return h, oo.Fh
}

func mkFS(t *testing.T, srv *fake.Server, cfg Config) *rawFS {
	t.Helper()
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "b"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "b", BlockSize: 1 << 20, MemCache: 256 << 20, MaxRange: 16 << 20})
	cfg.Index, cfg.Store = ix, bs
	if cfg.Limits == nil {
		cfg.Limits = prefetch.NewPolicy(128<<20, prefetch.DeviceLimits{}, nil)
	}
	return NewRawFileSystem(cfg).(*rawFS)
}

// TestBgzfWholeFileSmall: a CRAM at/below --bgzf-whole-file-max with a .crai
// sibling is prefetched whole on open (ruling 2) — no tier-2 ranges, and a read
// anywhere is a cache hit. The adaptive window is untouched (ruling 1).
func TestBgzfWholeFileSmall(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("aln.cram", make([]byte, 8<<20), now)
	srv.Put("aln.cram.crai", gz("0\t0\t1000\t0\t0\t1048576\n"), now)
	raw := mkFS(t, srv, Config{SmallFile: 1 << 20, PartsMax: 4 << 20, BgzfWholeFileMax: 512 << 20})

	h, fh := openHandle(t, raw, "aln.cram")
	if len(h.bgzfRanges) != 0 {
		t.Fatalf("small file should use whole-file prefetch, not tier-2 ranges; got %d ranges", len(h.bgzfRanges))
	}
	waitStableGets(srv)
	before := srv.GetCallCount()
	buf := make([]byte, 4096)
	raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: 6 << 20, Size: 4096}, buf)
	waitStableGets(srv)
	if after := srv.GetCallCount(); after != before {
		t.Fatalf("read after whole-file prefetch issued %d new GET(s); file not prefetched whole", after-before)
	}
}

// TestBgzfTier2LargeSlicePrecise: above the whole-file threshold, tier-2 loads
// slice-precise ranges from the .crai and a seek prefetches just that slice —
// starting at containerOffset+sliceOffset (not the whole container), and a
// follow-on read in the slice is a hit.
func TestBgzfTier2LargeSlicePrecise(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("big.cram", make([]byte, 8<<20), now)
	// Container @4 MiB, slice offset 64 KiB, size 1 MiB → slice [4259840, 5308416).
	srv.Put("big.cram.crai", gz(
		"0\t0\t1000\t0\t0\t1048576\n"+
			"0\t5000\t1000\t4194304\t65536\t1048576\n"), now)
	raw := mkFS(t, srv, Config{SmallFile: 1 << 20, PartsMax: 4 << 20, BgzfWholeFileMax: 1 << 20}) // 8 MiB > 1 MiB → large

	h, fh := openHandle(t, raw, "big.cram")
	if len(h.bgzfRanges) == 0 {
		t.Fatal("large file with .crai should load tier-2 ranges")
	}
	// Slice-precise: a range starts at container(4 MiB)+sliceOffset(64 KiB), not
	// at the container start.
	const sliceStart = 4194304 + 65536
	foundSlice, foundContainerStart := false, false
	for _, r := range h.bgzfRanges {
		if r.Start == sliceStart {
			foundSlice = true
		}
		if r.Start == 4194304 {
			foundContainerStart = true
		}
	}
	if !foundSlice {
		t.Fatalf("expected a slice-precise range starting at %d; got %v", sliceStart, h.bgzfRanges)
	}
	if foundContainerStart {
		t.Fatalf("range started at the container offset (whole-container over-fetch): %v", h.bgzfRanges)
	}

	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fh, Offset: uint64(off), Size: 4096}, buf)
	}
	readAt(sliceStart) // seek into the slice → tier-2 prefetches it
	waitStableGets(srv)
	before := srv.GetCallCount()
	readAt(sliceStart + 512<<10) // elsewhere in the slice → hit
	waitStableGets(srv)
	if after := srv.GetCallCount(); after != before {
		t.Fatalf("read within the prefetched slice issued %d new GET(s)", after-before)
	}
}

// TestBgzfNoIndex: a CRAM without a .crai sibling gets no bgzf treatment.
func TestBgzfNoIndex(t *testing.T) {
	srv := fake.New()
	srv.Put("plain.cram", make([]byte, 8<<20), time.Unix(1_700_000_000, 0))
	raw := mkFS(t, srv, Config{SmallFile: 1 << 20, BgzfWholeFileMax: 1 << 20})
	h, _ := openHandle(t, raw, "plain.cram")
	if len(h.bgzfRanges) != 0 {
		t.Fatalf("CRAM without an index got tier-2 ranges: %d", len(h.bgzfRanges))
	}
}

// TestBgzfMalformedIndexLargeTier1: a corrupt .crai on a large file yields no
// ranges (tier 1 only) and does not panic.
func TestBgzfMalformedIndexLargeTier1(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("x.cram", make([]byte, 8<<20), now)
	srv.Put("x.cram.crai", []byte("not a gzip stream"), now)
	raw := mkFS(t, srv, Config{SmallFile: 1 << 20, BgzfWholeFileMax: 1 << 20})
	h, _ := openHandle(t, raw, "x.cram")
	if len(h.bgzfRanges) != 0 {
		t.Fatalf("malformed .crai yielded ranges: %v", h.bgzfRanges)
	}
}
