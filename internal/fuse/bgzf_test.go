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

// openHandle opens rel (single component under root) and returns its handle.
func openHandle(t *testing.T, raw *rawFS, rel string) *fileHandle {
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
	return h
}

// TestBgzfTier1And2: opening a CRAM with a `.crai` sibling puts the handle in
// random-protection mode and loads the index's container ranges; a read that
// seeks into a container prefetches that whole container (tier-2), so a
// follow-on read inside it is a cache hit.
func TestBgzfTier1And2(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	const dataSize = 8 << 20
	srv.Put("aln.cram", make([]byte, dataSize), now)
	// Two containers: ref0 @0 (1 MiB) and ref0 @ 4 MiB (1 MiB).
	srv.Put("aln.cram.crai", gz(
		"0\t0\t1000\t0\t0\t1048576\n"+
			"0\t5000\t1000\t4194304\t0\t1048576\n"), now)

	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "b"}})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "b", BlockSize: 1 << 20, MemCache: 256 << 20, MaxRange: 16 << 20})
	pol := prefetch.NewPolicy(128<<20, prefetch.DeviceLimits{}, nil)
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, SmallFile: 1 << 20, Limits: pol}).(*rawFS)

	h := openHandle(t, raw, "aln.cram")
	if !h.randomProtect {
		t.Fatal("CRAM with a .crai sibling did not enter random-protection mode")
	}
	if len(h.bgzfRanges) == 0 {
		t.Fatal("no bgzf ranges loaded from .crai")
	}
	// Second container range starts at 4 MiB.
	found := false
	for _, r := range h.bgzfRanges {
		if r.Start == 4194304 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a range starting at 4 MiB; got %v", h.bgzfRanges)
	}

	// Seek into the 2nd container (offset 4 MiB) → tier-2 prefetches it whole.
	readAt := func(off int64) {
		buf := make([]byte, 4096)
		raw.Read(nil, &fuse.ReadIn{InHeader: fuse.InHeader{NodeId: 0}, Fh: fhOf(raw, h), Offset: uint64(off), Size: 4096}, buf)
	}
	readAt(4 << 20)
	waitStableGets(srv)
	before := srv.GetCallCount()
	// A read elsewhere in that container is now a cache hit (no new GET).
	readAt(4<<20 + 512<<10)
	waitStableGets(srv)
	if after := srv.GetCallCount(); after != before {
		t.Fatalf("read within the prefetched container issued %d new GET(s); tier-2 range prefetch not effective", after-before)
	}
}

// fhOf finds the fh for a handle (test helper).
func fhOf(raw *rawFS, h *fileHandle) uint64 {
	raw.mu.RLock()
	defer raw.mu.RUnlock()
	for fh, hh := range raw.handles {
		if hh == h {
			return fh
		}
	}
	return 0
}

// TestBgzfNoIndexNoProtection: a CRAM without a .crai sibling is handled
// normally (no random protection, no ranges).
func TestBgzfNoIndexNoProtection(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("plain.cram", make([]byte, 8<<20), now)
	ix, _ := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "b"}})
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "b", BlockSize: 1 << 20, MemCache: 64 << 20, MaxRange: 8 << 20})
	pol := prefetch.NewPolicy(64<<20, prefetch.DeviceLimits{}, nil)
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, SmallFile: 1 << 20, Limits: pol}).(*rawFS)

	h := openHandle(t, raw, "plain.cram")
	if h.randomProtect || len(h.bgzfRanges) != 0 {
		t.Fatalf("CRAM without an index got bgzf treatment: protect=%v ranges=%d", h.randomProtect, len(h.bgzfRanges))
	}
}

// TestBgzfMalformedIndexTier1: a corrupt .crai still detects (random protection
// on) but loads no ranges — tier 1 only, no panic.
func TestBgzfMalformedIndexTier1(t *testing.T) {
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	srv.Put("x.cram", make([]byte, 4<<20), now)
	srv.Put("x.cram.crai", []byte("not a gzip stream"), now)
	ix, _ := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "b"}})
	bs, _ := blockstore.New(srv, blockstore.Config{Bucket: "b", BlockSize: 1 << 20, MemCache: 64 << 20, MaxRange: 8 << 20})
	pol := prefetch.NewPolicy(64<<20, prefetch.DeviceLimits{}, nil)
	raw := NewRawFileSystem(Config{Index: ix, Store: bs, SmallFile: 1 << 20, Limits: pol}).(*rawFS)

	h := openHandle(t, raw, "x.cram")
	if !h.randomProtect {
		t.Fatal("detection should still set random protection with a present (if corrupt) index")
	}
	if len(h.bgzfRanges) != 0 {
		t.Fatalf("malformed .crai yielded ranges: %v", h.bgzfRanges)
	}
}
