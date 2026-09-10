// SPDX-License-Identifier: Apache-2.0

package index

import (
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func dirents(t *testing.T, r Reader, path string) []string {
	t.Helper()
	var names []string
	cur := uint64(0)
	for {
		ents, next, err := r.Readdir(path, cur, 64)
		if err != nil {
			t.Fatalf("Readdir(%q): %v", path, err)
		}
		if len(ents) == 0 {
			break
		}
		for _, e := range ents {
			n := e.Name
			if e.IsDir {
				n += "/"
			}
			names = append(names, n)
		}
		cur = next
	}
	sort.Strings(names)
	return names
}

// TestViewEqualsFreshPrefixIndex: a whole-bucket index viewed at a prefix and a
// freshly built prefix index return identical Readdir results (Step 2/3).
func TestViewEqualsFreshPrefixIndex(t *testing.T) {
	whole := build("", "a/b/x", "a/b/y", "a/b/sub/z", "a/c/other", "a/bx")
	view, err := whole.Root("a/b/")
	if err != nil {
		t.Fatalf("Root(a/b/): %v", err)
	}
	fresh := build("a/b/", "x", "y", "sub/z")

	for _, p := range []string{"/", "/sub"} {
		if v, f := dirents(t, view, p), dirents(t, fresh, p); !reflect.DeepEqual(v, f) {
			t.Fatalf("Readdir(%q): view=%v fresh=%v", p, v, f)
		}
	}
	// Root listing shows only children under a/b/ — not a/c/*, not the a/bx
	// sibling, not the "a/b" boundary object.
	if got := dirents(t, view, "/"); !reflect.DeepEqual(got, []string{"sub/", "x", "y"}) {
		t.Fatalf("view root = %v, want [sub/ x y]", got)
	}
	// Stat resolves mount-relative paths; the full key is reconstructed via Prefix.
	if fi, err := view.Stat("/x"); err != nil || fi.IsDir {
		t.Fatalf("Stat(/x) = %+v,%v", fi, err)
	}
	if view.Prefix() != "a/b/" {
		t.Fatalf("view.Prefix() = %q, want a/b/", view.Prefix())
	}
	if view.Len() != 3 { // x, y, sub/z
		t.Fatalf("view.Len() = %d, want 3", view.Len())
	}
}

// TestRootBoundary: a key "a/b" (object) is outside the "a/b/" mount root.
func TestRootBoundary(t *testing.T) {
	whole := build("", "a/b", "a/b/c", "a/b/d")
	view, err := whole.Root("a/b/")
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got := dirents(t, view, "/"); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Fatalf("root = %v, want [c d] (a/b object excluded)", got)
	}
}

// TestRootRejectsOutside: an index narrower than or disjoint from the requested
// root is rejected, naming both roots.
func TestRootRejectsOutside(t *testing.T) {
	ix := build("a/b/", "x", "y")
	for _, mp := range []string{"a/", "z/", "a/c/"} {
		if _, err := ix.Root(mp); !errors.Is(err, ErrRootOutside) {
			t.Errorf("Root(%q) err = %v, want ErrRootOutside", mp, err)
		}
	}
	// Equal root and a deeper sub-root are fine.
	if _, err := ix.Root("a/b/"); err != nil {
		t.Errorf("Root(a/b/) = %v, want ok", err)
	}
}

// TestRootZeroKeys: a prefix under which no keys exist is an error, not an empty
// mount.
func TestRootZeroKeys(t *testing.T) {
	whole := build("", "a/b/x", "a/c/y")
	if _, err := whole.Root("q/r/"); err == nil {
		t.Fatal("Root(q/r/) succeeded, want zero-key error")
	}
	// normalization: prefix, prefix/, /prefix/ all resolve the same.
	for _, mp := range []string{"a/b", "a/b/", "/a/b/"} {
		if _, err := whole.Root(mp); err != nil {
			t.Errorf("Root(%q) = %v, want ok", mp, err)
		}
	}
}

// TestViewNeighborhoodInRoot: Neighborhood never returns a key outside the root,
// and the keys it returns are mount-relative.
func TestViewNeighborhoodInRoot(t *testing.T) {
	whole := build("", "a/b/c0", "a/b/c1", "a/b/c2", "a/b/c3", "a/c/outside")
	view, err := whole.Root("a/b/")
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	sibs := view.Neighborhood("/c0", 10)
	var keys []string
	for _, s := range sibs {
		keys = append(keys, s.Key)
	}
	if !reflect.DeepEqual(keys, []string{"c1", "c2", "c3"}) {
		t.Fatalf("Neighborhood(/c0) = %v, want mount-relative [c1 c2 c3] (no a/c/outside)", keys)
	}
	// The last key in the root has no in-root siblings.
	if got := view.Neighborhood("/c3", 10); len(got) != 0 {
		t.Fatalf("Neighborhood(/c3) = %v, want empty (root boundary)", got)
	}
}

// TestViewTotalSizeConcurrent: TotalSize (what FUSE StatFs reads) must be
// race-free under concurrent access — it is computed once at Root() and read as
// an immutable field, not lazily set on first call (session 23).
func TestViewTotalSizeConcurrent(t *testing.T) {
	whole := build("", "a/b/x", "a/b/y", "a/b/sub/z", "a/c/other")
	r, err := whole.Root("a/b/")
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	want := r.TotalSize()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := r.TotalSize(); got != want {
					t.Errorf("TotalSize = %d, want %d", got, want)
					return
				}
				_ = r.Len()
			}
		}()
	}
	wg.Wait()
}
