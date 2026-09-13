// SPDX-License-Identifier: Apache-2.0

package index

import (
	"errors"
	"fmt"
	"strings"
)

// Reader is the read surface the FUSE layer and mount wiring use. Both a whole
// *Index and a prefix-rooted *View satisfy it, so one loaded index can back
// mounts rooted at different prefixes below it (#90).
type Reader interface {
	Bucket() string
	Prefix() string // the full bucket prefix this reader is rooted at ("" or "a/b/")
	Len() int
	TotalSize() int64
	Stat(path string) (FileInfo, error)
	Lookup(path string) (FileInfo, error)
	Readdir(path string, cursor uint64, n int) ([]Dirent, uint64, error)
	ETagHashOf(path string) uint64
	Position(path string) (int, bool)
	// ByInode resolves an inode to its "/"-rooted path (reverse of the build-time
	// inode assignment), for NFS handle resolution (#144). ok=false if no entry
	// has that inode (or, for a View, it lies outside the view).
	ByInode(ino uint64) (path string, ok bool)
	Neighborhood(path string, n int) []Sibling
	// BackingOf returns the CargoShip read-mapping for a virtual file, or ok=false
	// for an object-backed index. The backing travels with the entry, so a View
	// over a CargoShip index resolves it unchanged (#90).
	BackingOf(path string) (Backing, bool)
}

// static assertions.
var (
	_ Reader = (*Index)(nil)
	_ Reader = (*View)(nil)
)

// ErrRootOutside is returned by Root when the requested mount root is not
// contained by the index's own root (the index is narrower than, or disjoint
// from, the request).
var ErrRootOutside = errors.New("index: requested mount root not contained by index root")

// View presents an *Index rooted at a prefix at or below the index's build
// root. It strips `sub` (the requested root relative to the index's build root)
// on the way in and out, so FUSE sees only paths beneath the mount root. The
// [lo,hi) arena range is the position span of keys under `sub`, found by binary
// search; sub == "" is a pass-through view equal to the index itself.
type View struct {
	ix     *Index
	sub    string // requested root relative to ix.prefix, "" or "c/d/"
	root   string // full bucket prefix: ix.prefix + sub
	lo, hi int    // arena range of keys under sub
	total  int64  // sum of sizes over [lo,hi), computed once at Root() (immutable)
}

// Root returns a Reader rooted at mountPrefix (a full bucket prefix). It
// requires the index's build root to be a parent of (or equal to) mountPrefix;
// otherwise ErrRootOutside. A mount root under which no keys exist is an error.
func (ix *Index) Root(mountPrefix string) (Reader, error) {
	mp := normalizePrefix(mountPrefix)
	br := ix.prefix
	if mp == br {
		return ix, nil // whole index (pass-through)
	}
	if br != "" && !strings.HasPrefix(mp, br) {
		return nil, fmt.Errorf("%w: index root %q, mount root %q", ErrRootOutside, br, mp)
	}
	sub := strings.TrimPrefix(mp, br) // relative to the index's build root
	lo := ix.lowerBound(sub)
	hi := ix.lowerBound(prefixUpperBound(sub))
	if hi <= lo {
		return nil, fmt.Errorf("no objects under mount root %q", mp)
	}
	// Sum sizes once, here, so TotalSize is an immutable field read — StatFs is
	// called concurrently and a lazy set would race (no lock on the read path).
	var total int64
	for i := lo; i < hi; i++ {
		total += int64(ix.sizes[i])
	}
	return &View{ix: ix, sub: sub, root: mp, lo: lo, hi: hi, total: total}, nil
}

// prefixUpperBound returns the smallest string greater than every string with
// prefix p (p with its last byte incremented), or "" (unbounded) if p is empty
// or all 0xff. For a "c/d/"-style prefix the last byte is '/', so the bound is
// "c/d0" — everything under "c/d/" sorts before it.
func prefixUpperBound(p string) string {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "" // sorts after everything (lowerBound returns Len)
}

// abs maps a mount-relative slash path to the index-relative (build-root)
// slash path, handling the root ("/" -> the sub directory itself, no trailing
// slash so Stat/dirLookup match).
func (v *View) abs(path string) string {
	r := toRel(path)
	if r == "" {
		return "/" + strings.TrimSuffix(v.sub, "/")
	}
	return "/" + v.sub + r
}

func (v *View) Bucket() string { return v.ix.bucket }
func (v *View) Prefix() string { return v.root }
func (v *View) Len() int       { return v.hi - v.lo }

func (v *View) TotalSize() int64 { return v.total }

func (v *View) Stat(path string) (FileInfo, error)    { return v.ix.Stat(v.abs(path)) }
func (v *View) Lookup(path string) (FileInfo, error)  { return v.ix.Stat(v.abs(path)) }
func (v *View) ETagHashOf(path string) uint64         { return v.ix.ETagHashOf(v.abs(path)) }
func (v *View) Position(path string) (int, bool)      { return v.ix.Position(v.abs(path)) }
func (v *View) BackingOf(path string) (Backing, bool) { return v.ix.BackingOf(v.abs(path)) }

func (v *View) Readdir(path string, cursor uint64, n int) ([]Dirent, uint64, error) {
	return v.ix.Readdir(v.abs(path), cursor, n)
}

// Neighborhood delegates to the index at the absolute path, then strips `sub`
// from each returned key so callers get mount-relative keys. Siblings are
// same-directory direct children, so they are always under `sub` — the view
// never returns a key outside the root.
func (v *View) Neighborhood(path string, n int) []Sibling {
	sibs := v.ix.Neighborhood(v.abs(path), n)
	if v.sub == "" {
		return sibs
	}
	out := sibs[:0]
	for _, s := range sibs {
		if !strings.HasPrefix(s.Key, v.sub) {
			continue // defensive: never surface a key outside the root
		}
		s.Key = strings.TrimPrefix(s.Key, v.sub)
		out = append(out, s)
	}
	return out
}
