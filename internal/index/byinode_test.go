// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"testing"
	"time"

	"github.com/scttfrdmn/lith/internal/s3client/fake"
)

func buildFixture(t *testing.T, prefix string, keys []string) *Index {
	t.Helper()
	srv := fake.New()
	now := time.Unix(1_700_000_000, 0)
	for _, k := range keys {
		srv.PutString(k, "x", now)
	}
	ix, err := BuildFromList(context.Background(), srv, ListOptions{
		Options: Options{Bucket: "bkt", Prefix: prefix},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return ix
}

// TestByInodeRoundTripAllKeys: every file and directory round-trips
// path -> inode -> path, including the root directory.
func TestByInodeRoundTripAllKeys(t *testing.T) {
	ix := buildFixture(t, "data", []string{
		"data/a.txt", "data/b/c.txt", "data/b/d.txt", "data/b/e/f.txt", "data/g.txt",
	})
	// Root directory.
	rootFI, _ := ix.Stat("/")
	if p, ok := ix.ByInode(rootFI.Ino); !ok || p != "/" {
		t.Errorf("root inode -> %q,%v want /,true", p, ok)
	}
	// Every file.
	for _, rel := range []string{"/a.txt", "/b/c.txt", "/b/d.txt", "/b/e/f.txt", "/g.txt"} {
		fi, err := ix.Stat(rel)
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		got, ok := ix.ByInode(fi.Ino)
		if !ok || got != rel {
			t.Errorf("ByInode(%d) = %q,%v; want %q", fi.Ino, got, ok, rel)
		}
	}
	// Every directory.
	for _, rel := range []string{"/b", "/b/e"} {
		fi, err := ix.Stat(rel)
		if err != nil || !fi.IsDir {
			t.Fatalf("stat dir %s: %v", rel, err)
		}
		got, ok := ix.ByInode(fi.Ino)
		if !ok || got != rel {
			t.Errorf("ByInode(dir %d) = %q,%v; want %q", fi.Ino, got, ok, rel)
		}
	}
	// An inode nobody owns.
	if p, ok := ix.ByInode(0xdead_beef_dead_beef); ok {
		t.Errorf("unknown inode resolved to %q", p)
	}
}

// TestByInodeCollisionsResolved forces inode hash collisions (all keys hash to
// the same base) and checks every key still round-trips — the probe table keeps
// inodes unique, and ByInode inverts them.
func TestByInodeCollisionsResolved(t *testing.T) {
	orig := hashKey
	hashKey = func(string) uint64 { return 42 } // everything collides
	defer func() { hashKey = orig }()

	ix := buildFixture(t, "", []string{"a", "b", "c", "sub/d", "sub/e"})
	if ix.collisions == 0 {
		t.Fatal("expected forced inode collisions")
	}
	seen := map[uint64]string{}
	for _, rel := range []string{"/a", "/b", "/c", "/sub/d", "/sub/e", "/sub"} {
		fi, err := ix.Stat(rel)
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if prev, dup := seen[fi.Ino]; dup {
			t.Fatalf("inode %d shared by %s and %s", fi.Ino, prev, rel)
		}
		seen[fi.Ino] = rel
		if got, ok := ix.ByInode(fi.Ino); !ok || got != rel {
			t.Errorf("ByInode(%d) = %q,%v; want %q", fi.Ino, got, ok, rel)
		}
	}
}

// TestByInodeViewBoundaries: through a sub-root view, only inodes under the view
// resolve, and they resolve to view-relative paths; inodes outside are not found.
func TestByInodeViewBoundaries(t *testing.T) {
	ix := buildFixture(t, "", []string{"top.txt", "sub/a.txt", "sub/deep/b.txt", "other/z.txt"})
	r, err := ix.Root("sub")
	if err != nil {
		t.Fatalf("Root(sub): %v", err)
	}
	v := r.(*View)

	// In-view file resolves view-relative.
	fi, _ := ix.Stat("/sub/a.txt")
	if got, ok := v.ByInode(fi.Ino); !ok || got != "/a.txt" {
		t.Errorf("view ByInode(sub/a.txt) = %q,%v; want /a.txt", got, ok)
	}
	fi, _ = ix.Stat("/sub/deep/b.txt")
	if got, ok := v.ByInode(fi.Ino); !ok || got != "/deep/b.txt" {
		t.Errorf("view ByInode(sub/deep/b.txt) = %q,%v; want /deep/b.txt", got, ok)
	}
	// The view root directory.
	fi, _ = ix.Stat("/sub")
	if got, ok := v.ByInode(fi.Ino); !ok || got != "/" {
		t.Errorf("view ByInode(sub) = %q,%v; want /", got, ok)
	}
	// Out-of-view inodes are not found through the view.
	fi, _ = ix.Stat("/top.txt")
	if _, ok := v.ByInode(fi.Ino); ok {
		t.Error("view resolved an out-of-view file inode")
	}
	fi, _ = ix.Stat("/other/z.txt")
	if _, ok := v.ByInode(fi.Ino); ok {
		t.Error("view resolved an inode in a sibling subtree")
	}
}

// TestByInodeStableForSamePath documents that inodes are content-hash-stable:
// a path present in two different key sets keeps the same inode. This is WHY
// #144's staleness guard is the index-sha root id in the handle, not the inode —
// the inode alone cannot detect a rebuild. (The sha guard is tested in the
// gateway.)
func TestByInodeStableForSamePath(t *testing.T) {
	keysA := []string{"d/keep.txt", "d/x.txt"}
	keysB := []string{"d/keep.txt", "d/y.txt", "d/z.txt"} // different set, keep.txt survives
	ixA := buildFixture(t, "d", keysA)
	ixB := buildFixture(t, "d", keysB)
	a, _ := ixA.Stat("/keep.txt")
	b, _ := ixB.Stat("/keep.txt")
	if a.Ino != b.Ino {
		t.Fatalf("keep.txt inode changed across rebuild (%d vs %d) — unexpected without a collision", a.Ino, b.Ino)
	}
	if p, ok := ixB.ByInode(a.Ino); !ok || p != "/keep.txt" {
		t.Errorf("ByInode stable path = %q,%v; want /keep.txt", p, ok)
	}
}

// TestByInodeDiffersOnCollisionShift: when a key set change shifts collision
// probing, a surviving path's inode changes, so an old inode resolves to a
// different path (or nothing) in the rebuilt index — the concrete staleness the
// #144 root-id sha must guard against even when inodes are reused.
func TestByInodeDiffersOnCollisionShift(t *testing.T) {
	orig := hashKey
	// "a" and "b" collide; the second one built probes to base+1.
	hashKey = func(s string) uint64 {
		if s == "c/a" || s == "c/b" {
			return 100
		}
		return orig(s)
	}
	defer func() { hashKey = orig }()

	_ = buildFixture(t, "c", []string{"c/a", "c/b"}) // A: a=100, b=101 (probed)
	ixB := buildFixture(t, "c", []string{"c/b"})     // B: b=100 now (no collision)
	fiB, _ := ixB.Stat("/b")
	if fiB.Ino != 100 {
		t.Fatalf("b inode in B = %d, want 100", fiB.Ino)
	}
	// Inode 101 was /b in A; in B nothing owns 101 -> a stale A-handle is caught.
	if p, ok := ixB.ByInode(101); ok {
		t.Errorf("stale inode 101 resolved in rebuilt index to %q; expected not-found", p)
	}
}
