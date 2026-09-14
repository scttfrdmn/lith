// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"context"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

// buildVersionIndex builds an index over a fake bucket with the given keys — a
// stand-in for one published version.
func buildVersionIndex(t *testing.T, keys map[string]string) *index.Index {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	for k, v := range keys {
		srv.PutString(k, v, now)
	}
	ix, err := index.BuildFromList(context.Background(), srv, index.ListOptions{Options: index.Options{Bucket: "bkt"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	return ix
}

func lookupStatus(f *rawFS, name string) fuse.Status {
	var eo fuse.EntryOut
	return f.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, name, &eo)
}

// TestSwapIndexAffectsLookups: SwapIndex atomically replaces the index used for
// new lookups (#167 refresh). After the swap, keys unique to v2 resolve and keys
// unique to v1 do not.
func TestSwapIndexAffectsLookups(t *testing.T) {
	ix1 := buildVersionIndex(t, map[string]string{"shared.txt": "aaa", "only-v1.txt": "x"})
	ix2 := buildVersionIndex(t, map[string]string{"shared.txt": "bbbbbb", "only-v2.txt": "y"})
	bs, err := blockstore.New(fake.New(), blockstore.Config{Bucket: "bkt", BlockSize: 4, MemCache: 1 << 20, MaxRange: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	root1, err := ix1.Root("")
	if err != nil {
		t.Fatal(err)
	}
	f := NewRawFileSystem(Config{Index: root1, Store: bs, UID: 1000, GID: 1000}).(*rawFS)

	if s := lookupStatus(f, "only-v1.txt"); s != fuse.OK {
		t.Fatalf("v1: only-v1.txt lookup = %v, want OK", s)
	}
	if s := lookupStatus(f, "only-v2.txt"); s == fuse.OK {
		t.Fatal("only-v2.txt resolved before swap")
	}

	root2, err := ix2.Root("")
	if err != nil {
		t.Fatal(err)
	}
	f.SwapIndex(root2)

	if s := lookupStatus(f, "only-v2.txt"); s != fuse.OK {
		t.Fatalf("v2: only-v2.txt lookup = %v after swap, want OK", s)
	}
	if s := lookupStatus(f, "only-v1.txt"); s == fuse.OK {
		t.Fatal("only-v1.txt still resolved after swap")
	}
}

// TestHandlePinnedAcrossSwap: a handle opened against v1 keeps the backing it
// captured at open (its key and size) after a swap to v2, even though v2's version
// of the same file differs. This is the correctness contract of the swap — an
// in-flight read completes against its own version's (immutable) objects; only new
// lookups see v2.
func TestHandlePinnedAcrossSwap(t *testing.T) {
	ix1 := buildVersionIndex(t, map[string]string{"shared.txt": "aaa"})              // size 3
	ix2 := buildVersionIndex(t, map[string]string{"shared.txt": "bbbbbbbbbbbbbbbb"}) // size 16
	bs, err := blockstore.New(fake.New(), blockstore.Config{Bucket: "bkt", BlockSize: 4, MemCache: 1 << 20, MaxRange: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	root1, _ := ix1.Root("")
	f := NewRawFileSystem(Config{Index: root1, Store: bs, UID: 1000, GID: 1000, SmallFile: 0, PartsMax: 0}).(*rawFS)

	h, _ := openHandle(t, f, "shared.txt")
	gotKey, gotSize := h.key.Key, h.size
	if gotSize != 3 {
		t.Fatalf("opened size = %d, want 3 (v1)", gotSize)
	}

	root2, _ := ix2.Root("")
	f.SwapIndex(root2)

	// The open handle is unchanged — it does not follow the swap.
	if h.key.Key != gotKey || h.size != gotSize {
		t.Fatalf("handle changed across swap: key %q->%q size %d->%d", gotKey, h.key.Key, gotSize, h.size)
	}
	// A fresh lookup DOES see v2's larger file.
	var eo fuse.EntryOut
	if s := f.Lookup(nil, &fuse.InHeader{NodeId: fuse.FUSE_ROOT_ID}, "shared.txt", &eo); s != fuse.OK {
		t.Fatalf("post-swap lookup: %v", s)
	}
	if eo.Attr.Size != 16 {
		t.Errorf("post-swap lookup size = %d, want 16 (v2)", eo.Attr.Size)
	}
}

// TestInodeStableAcrossVersions: inodes are path hashes, so a file present in two
// versions keeps its inode across a bump — the property that lets `find`/`rsync`
// tolerate a hot-swap (a cached (dev,ino) stays valid). Would silently break if
// inode assignment ever became position-based.
func TestInodeStableAcrossVersions(t *testing.T) {
	ix1 := buildVersionIndex(t, map[string]string{"shared.txt": "aaa", "a.txt": "1", "dir/x": "2"})
	ix2 := buildVersionIndex(t, map[string]string{"shared.txt": "totally different", "b.txt": "3", "c.txt": "4", "dir/x": "2"})

	fi1, err := ix1.Stat("/shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	fi2, err := ix2.Stat("/shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi1.Ino != fi2.Ino {
		t.Errorf("shared.txt inode changed across versions: v1=%d v2=%d", fi1.Ino, fi2.Ino)
	}
	// A file in a subdirectory present in both versions is stable too.
	d1, _ := ix1.Stat("/dir/x")
	d2, _ := ix2.Stat("/dir/x")
	if d1.Ino != d2.Ino {
		t.Errorf("dir/x inode changed across versions: v1=%d v2=%d", d1.Ino, d2.Ino)
	}
}
