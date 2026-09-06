// SPDX-License-Identifier: Apache-2.0

package index

import (
	"fmt"
	"sort"
	"testing"
	"testing/fstest"
)

// build makes an index from already-relative keys, each given a distinct
// size/mtime so metadata round-trips can be checked.
func build(prefix string, keys ...string) *Index {
	e := make([]Entry, len(keys))
	for i, k := range keys {
		e[i] = Entry{Key: k, Size: int64(i + 1), MTime: int64(i+1) * 1e9, ETagHash: uint64(i + 1)}
	}
	return Build(e, Options{Bucket: "bkt", Prefix: prefix})
}

// readAll pages a directory to completion and returns its entries by name.
func readAll(t *testing.T, ix *Index, path string) map[string]Dirent {
	t.Helper()
	out := map[string]Dirent{}
	var cursor uint64
	for {
		ents, next, err := ix.Readdir(path, cursor, 3)
		if err != nil {
			t.Fatalf("Readdir(%q): %v", path, err)
		}
		if len(ents) == 0 {
			break
		}
		for _, e := range ents {
			if _, dup := out[e.Name]; dup {
				t.Fatalf("Readdir(%q): duplicate entry %q", path, e.Name)
			}
			out[e.Name] = e
		}
		cursor = next
	}
	return out
}

func names(m map[string]Dirent) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

func TestReaddirDerivation(t *testing.T) {
	cases := []struct {
		name     string
		keys     []string
		dir      string
		want     []string // sorted names
		wantDirs []string // which of want are directories
	}{
		{
			name: "empty prefix root",
			keys: []string{"a", "b", "c"},
			dir:  "/",
			want: []string{"a", "b", "c"},
		},
		{
			name:     "nested prefixes collapse to one dir",
			keys:     []string{"d/x", "d/y", "d/z/w", "e"},
			dir:      "/",
			want:     []string{"d", "e"},
			wantDirs: []string{"d"},
		},
		{
			name:     "readdir into nested dir",
			keys:     []string{"d/x", "d/y", "d/z/w"},
			dir:      "/d",
			want:     []string{"x", "y", "z"},
			wantDirs: []string{"z"},
		},
		{
			name:     "zero-length folder key is a directory",
			keys:     []string{"folder/", "folder/f", "top"},
			dir:      "/",
			want:     []string{"folder", "top"},
			wantDirs: []string{"folder"},
		},
		{
			name:     "empty folder marker only",
			keys:     []string{"empty/", "x"},
			dir:      "/",
			want:     []string{"empty", "x"},
			wantDirs: []string{"empty"},
		},
		{
			name:     "key and same-named prefix: directory wins",
			keys:     []string{"a", "a/b"},
			dir:      "/",
			want:     []string{"a"},
			wantDirs: []string{"a"},
		},
		{
			name:     "trailing slash keys collapse",
			keys:     []string{"p/", "p/q/", "p/q/r"},
			dir:      "/p",
			want:     []string{"q"},
			wantDirs: []string{"q"},
		},
		{
			name: "invalid components dropped",
			keys: []string{"good", "bad//x", "./dot", "../up", "ok/leaf"},
			dir:  "/",
			want: []string{"good", "ok"},
			// "ok" is a dir; "good" a file.
			wantDirs: []string{"ok"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ix := build("", tc.keys...)
			got := readAll(t, ix, tc.dir)
			if g := names(got); !equalStrings(g, tc.want) {
				t.Fatalf("names = %v, want %v", g, tc.want)
			}
			dirset := map[string]bool{}
			for _, d := range tc.wantDirs {
				dirset[d] = true
			}
			for name, d := range got {
				if d.IsDir != dirset[name] {
					t.Errorf("entry %q IsDir=%v, want %v", name, d.IsDir, dirset[name])
				}
				if d.Ino == 0 {
					t.Errorf("entry %q has zero inode", name)
				}
			}
		})
	}
}

func TestLookupAndStat(t *testing.T) {
	ix := build("", "dir/a", "dir/b", "dir/sub/c", "top.txt", "folder/")

	// Root is a directory.
	if fi, err := ix.Stat("/"); err != nil || !fi.IsDir || fi.Ino != rootIno {
		t.Fatalf("root stat: fi=%+v err=%v", fi, err)
	}
	// A file resolves with its size and nlink 1.
	fi, err := ix.Stat("/top.txt")
	if err != nil || fi.IsDir || fi.Nlink != 1 {
		t.Fatalf("file stat: fi=%+v err=%v", fi, err)
	}
	// A derived directory resolves; nlink counts subdirs (dir/sub) -> 2+1.
	fi, err = ix.Stat("/dir")
	if err != nil || !fi.IsDir {
		t.Fatalf("dir stat: fi=%+v err=%v", fi, err)
	}
	if fi.Nlink != 3 {
		t.Errorf("/dir nlink = %d, want 3", fi.Nlink)
	}
	// A zero-length folder marker is a directory.
	if fi, err := ix.Stat("/folder"); err != nil || !fi.IsDir {
		t.Fatalf("folder marker stat: fi=%+v err=%v", fi, err)
	}
	// Missing path.
	if _, err := ix.Stat("/nope"); err != ErrNotExist {
		t.Fatalf("missing path err = %v, want ErrNotExist", err)
	}
}

func TestReaddirMissingDir(t *testing.T) {
	ix := build("", "a")
	if _, _, err := ix.Readdir("/nope", 0, 10); err != ErrNotExist {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestSanitize(t *testing.T) {
	valid := []string{"a", "a/b", "a/b/c", "folder/", "a.b/c-d", "x/"}
	invalid := []string{"", "/", "a//b", "a/./b", "a/../b", ".", "..", "a//", "with\x00nul"}
	for _, k := range valid {
		if !sanitize(k) {
			t.Errorf("sanitize(%q) = false, want true", k)
		}
	}
	for _, k := range invalid {
		if sanitize(k) {
			t.Errorf("sanitize(%q) = true, want false", k)
		}
	}
}

func TestDroppedAndShadowedCounts(t *testing.T) {
	ix := build("", "good", "bad//x", "a", "a/b", "ok")
	dropped, shadowed, _ := ix.Stats()
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if shadowed != 1 { // "a" shadowed by "a/b"
		t.Errorf("shadowed = %d, want 1", shadowed)
	}
}

func TestInodesUniqueAndDeterministic(t *testing.T) {
	keys := []string{"a", "b", "c/d", "c/e", "long/nested/path/file"}
	ix1 := build("p", keys...)
	ix2 := build("p", keys...)
	seen := map[uint64]string{}
	for i := 0; i < ix1.Len(); i++ {
		ino := ix1.inos[i]
		if ino == 0 || ino == rootIno {
			t.Fatalf("key %q got reserved inode %d", ix1.key(i), ino)
		}
		if prev, dup := seen[ino]; dup {
			t.Fatalf("inode collision %d between %q and %q", ino, prev, ix1.key(i))
		}
		seen[ino] = ix1.key(i)
		if ix2.inos[i] != ino {
			t.Fatalf("inode for %q not deterministic: %d vs %d", ix1.key(i), ino, ix2.inos[i])
		}
	}
}

// TestInodeCollisionFallback exercises the probing path directly by seeding
// the used set with the base hash an entry would receive.
func TestInodeCollisionFallback(t *testing.T) {
	ix := &Index{prefix: ""}
	used := map[uint64]struct{}{0: {}, rootIno: {}}
	base := hashKey("collide")
	// Pre-take the natural hash so the next assignment must probe.
	used[base] = struct{}{}
	got := ix.assignIno(base, used)
	if got == base {
		t.Fatalf("expected probing away from taken hash %d", base)
	}
	if ix.collisions != 1 {
		t.Errorf("collisions = %d, want 1", ix.collisions)
	}
	if _, dup := used[got]; !dup {
		t.Errorf("probed inode %d not recorded as used", got)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	ix := build("some/prefix", "dir/a", "dir/b", "dir/sub/c", "top", "folder/")
	b1 := ix.Marshal()
	ix2, err := Unmarshal(b1)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	b2 := ix2.Marshal()
	if len(b1) != len(b2) {
		t.Fatalf("re-marshal length differs: %d vs %d", len(b1), len(b2))
	}
	for i := range b1 {
		if b1[i] != b2[i] {
			t.Fatalf("re-marshal differs at byte %d", i)
		}
	}
	// Queries agree.
	if ix2.Bucket() != "bkt" || ix2.Prefix() != "some/prefix/" {
		t.Errorf("bucket/prefix = %q/%q", ix2.Bucket(), ix2.Prefix())
	}
	fi1, _ := ix.Stat("/top")
	fi2, _ := ix2.Stat("/top")
	if fi1 != fi2 {
		t.Errorf("stat mismatch after round trip: %+v vs %+v", fi1, fi2)
	}
}

func TestSaveAndOpenMmap(t *testing.T) {
	ix := build("", "a/b", "a/c", "d")
	path := t.TempDir() + "/idx.lithidx"
	if err := ix.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, closeFn, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = closeFn() }()
	if loaded.Len() != ix.Len() {
		t.Fatalf("Len = %d, want %d", loaded.Len(), ix.Len())
	}
	got := readAll(t, loaded, "/a")
	if g := names(got); !equalStrings(g, []string{"b", "c"}) {
		t.Errorf("mmap readdir /a = %v", g)
	}
	if fi, err := loaded.Stat("/d"); err != nil || fi.Size != 3 {
		t.Errorf("mmap stat /d: fi=%+v err=%v", fi, err)
	}
}

// TestReaddirConstantMemory verifies that a single Readdir page allocates in
// proportion to the page size, not to the total number of children (2M here).
func TestReaddirConstantMemory(t *testing.T) {
	const total = 2_000_000
	e := make([]Entry, total)
	for i := range e {
		e[i] = Entry{Key: fmt.Sprintf("big/%08d", i), Size: 1}
	}
	ix := Build(e, Options{Bucket: "b"})

	// Page from a cursor deep in the directory; one page of 1000.
	deep := ix.lowerBound("big/01000000")
	cursor := uint64(deep + 1)
	allocs := testing.AllocsPerRun(5, func() {
		if ents, _, err := ix.Readdir("/big", cursor, 1000); err != nil || len(ents) != 1000 {
			t.Fatalf("Readdir: n=%d err=%v", len(ents), err)
		}
	})
	// ~1000 name strings + a slice; nowhere near 2M. A generous ceiling that
	// still fails if the whole directory were materialized.
	if allocs > 3000 {
		t.Fatalf("Readdir(1000) allocations = %v, want <= 3000 (must not materialize all %d children)", allocs, total)
	}

	// The full directory can still be enumerated exactly once.
	var count, c uint64
	for {
		ents, next, err := ix.Readdir("/big", c, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) == 0 {
			break
		}
		count += uint64(len(ents))
		c = next
	}
	if count != total {
		t.Fatalf("enumerated %d children, want %d", count, total)
	}
}

// TestAgainstTestFS cross-checks derived listings against a known tree using
// the standard library's fstest as an independent oracle for a small case.
func TestAgainstTestFS(t *testing.T) {
	keys := []string{"a/b/c", "a/b/d", "a/e", "f"}
	ix := build("", keys...)
	oracle := fstest.MapFS{}
	for _, k := range keys {
		oracle[k] = &fstest.MapFile{Data: []byte("x")}
	}
	// Compare root listing.
	got := names(readAll(t, ix, "/"))
	oents, _ := oracle.ReadDir(".")
	var want []string
	for _, e := range oents {
		want = append(want, e.Name())
	}
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("root listing = %v, oracle = %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
