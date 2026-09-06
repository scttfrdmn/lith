// SPDX-License-Identifier: Apache-2.0

package index

import (
	"strings"
	"testing"
	"time"
)

// TestSharedInodeNamespace forces directory/file inode collisions using a
// reduced-range hash seam and asserts every inode (files and directories) is
// globally distinct, with collisions counted. A literal brute-forced 64-bit
// xxh3 collision is infeasible in a unit test, so we shrink the hash space to
// exercise the same shared-namespace fallback path deterministically.
func TestSharedInodeNamespace(t *testing.T) {
	orig := hashKey
	// 6-bit hash: guarantees heavy collisions across the keys and dir paths.
	hashKey = func(s string) uint64 { return orig(s) & 0x3f }
	defer func() { hashKey = orig }()

	keys := []string{
		"a", "b", "c/d", "c/e", "c/f/g", "h/i", "h/j/k", "l/m/n/o", "p", "q/r",
	}
	e := make([]Entry, len(keys))
	for i, k := range keys {
		e[i] = Entry{Key: k, Size: int64(i + 1), MTime: int64(i+1) * 1e9}
	}
	ix := Build(e, Options{Bucket: "b"})

	seen := map[uint64]string{}
	// File inodes.
	for i := 0; i < ix.Len(); i++ {
		ino := ix.inos[i]
		if ino == 0 {
			t.Fatalf("file %q has zero inode", ix.key(i))
		}
		if prev, dup := seen[ino]; dup {
			t.Fatalf("inode %d shared by file %q and %q", ino, prev, ix.key(i))
		}
		seen[ino] = "file:" + ix.key(i)
	}
	// Directory inodes (including root).
	for i := 0; i < ix.dirCount(); i++ {
		ino := ix.dirInos[i]
		if ino == 0 {
			t.Fatalf("dir %q has zero inode", ix.dirPath(i))
		}
		if prev, dup := seen[ino]; dup {
			t.Fatalf("inode %d shared by dir %q and %q", ino, ix.dirPath(i), prev)
		}
		seen[ino] = "dir:" + ix.dirPath(i)
	}
	if _, _, c := ix.Stats(); c == 0 {
		t.Fatal("expected inode collisions to be forced and counted")
	}
	// Root keeps its reserved inode.
	if fi, _ := ix.Stat("/"); fi.Ino != rootIno {
		t.Errorf("root inode = %d, want %d", fi.Ino, rootIno)
	}
}

// TestDirMtimeMaxOverSubtree verifies a directory's mtime is the max over its
// whole subtree, not just its direct children.
func TestDirMtimeMaxOverSubtree(t *testing.T) {
	sec := func(n int64) int64 { return time.Unix(n, 0).UnixNano() }
	// top/ has a direct child at t=100 and a deep descendant at t=500.
	e := []Entry{
		{Key: "top/a", MTime: sec(100)},
		{Key: "top/mid/deep", MTime: sec(500)},
		{Key: "top/mid/shallow", MTime: sec(200)},
		{Key: "other", MTime: sec(50)},
	}
	ix := Build(e, Options{Bucket: "b", BuildTime: time.Unix(1, 0)})

	mustDir := func(path string, wantSec int64) {
		t.Helper()
		fi, err := ix.Stat(path)
		if err != nil || !fi.IsDir {
			t.Fatalf("%s: fi=%+v err=%v", path, fi, err)
		}
		if got := fi.MTime.Unix(); got != wantSec {
			t.Errorf("%s mtime = %d, want %d", path, got, wantSec)
		}
	}
	mustDir("/top", 500)     // max over the whole subtree, not just direct child (100)
	mustDir("/top/mid", 500) // max of deep(500) and shallow(200)
	mustDir("/", 500)        // root spans everything
}

// TestDirMtimeFallbackToBuildTime verifies an empty folder marker with no dated
// descendant falls back to the build time.
func TestDirMtimeFallbackToBuildTime(t *testing.T) {
	bt := time.Unix(123456, 0)
	e := []Entry{
		{Key: "empty/", MTime: 0},    // marker, no dated descendant
		{Key: "x", MTime: 999 * 1e9}, // unrelated file
	}
	ix := Build(e, Options{Bucket: "b", BuildTime: bt})
	fi, err := ix.Stat("/empty")
	if err != nil || !fi.IsDir {
		t.Fatalf("/empty: fi=%+v err=%v", fi, err)
	}
	if fi.MTime.Unix() != bt.Unix() {
		t.Errorf("/empty mtime = %d, want build time %d", fi.MTime.Unix(), bt.Unix())
	}
}

// TestFormatV1Rejected verifies a v1 image is rejected with a rebuild message.
func TestFormatV1Rejected(t *testing.T) {
	ix := build("", "a/b", "c")
	img := ix.Marshal()
	// Rewrite the version field (offset 8) to 1.
	img[8] = 1
	img[9], img[10], img[11] = 0, 0, 0
	if _, err := Unmarshal(img); err == nil {
		t.Fatal("expected v1 image to be rejected")
	} else if !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("error should mention rebuild, got %v", err)
	}
}
