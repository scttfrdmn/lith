// SPDX-License-Identifier: Apache-2.0

package index

import (
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zeebo/xxh3"
)

// Entry is one object collected by a builder before finalization. Key is the
// relative key (the mount prefix already stripped).
type Entry struct {
	Key      string
	Size     int64
	MTime    int64 // Unix nanoseconds
	ETagHash uint64
}

// Options controls index construction.
type Options struct {
	Bucket string
	// Prefix is the mount root within the bucket. It is normalized to end in
	// "/" (or be empty) and is not part of the stored relative keys.
	Prefix string
	// Exec, when set, makes files report mode 0555 instead of 0444.
	Exec bool
	// BuildTime is the fallback mtime for directories with no dated descendant.
	// Zero means time.Now() at build.
	BuildTime time.Time
	// Logger receives build progress and warnings; nil disables logging.
	Logger *slog.Logger
}

// hashKey is the 64-bit hash used to derive inodes from keys and directory
// paths. It is a package variable so tests can substitute a reduced-range hash
// to exercise the collision-fallback path deterministically.
var hashKey = func(s string) uint64 { return xxh3.HashString(s) }

// HashETag returns the 64-bit hash lith stores for an ETag string.
func HashETag(etag string) uint64 { return xxh3.HashString(etag) }

// sanitize reports whether a relative key is a valid POSIX path. A single
// trailing "/" (a folder marker) is allowed; embedded "//", "." or ".."
// components, an empty key, or any C0 control byte (< 0x20, including NUL,
// newline, CR, ESC, backspace) anywhere in the key are rejected — the latter so
// terminal-escape / argument-injection names never enter the namespace.
func sanitize(rel string) bool {
	if rel == "" {
		return false
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] < 0x20 {
			return false
		}
	}
	s := strings.TrimSuffix(rel, "/")
	if s == "" {
		return false
	}
	for _, c := range strings.Split(s, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

// Build finalizes a set of collected entries into an immutable Index: it
// sanitizes keys, sorts them, drops objects shadowed by a same-named
// directory, derives the directory table, assigns inodes to files and
// directories from a single collision namespace (xxh3 with sequential
// fallback), computes each directory's mtime as the max over its descendants,
// and packs everything into arenas with parallel metadata arrays.
func Build(entries []Entry, opts Options) *Index {
	bt := opts.BuildTime
	if bt.IsZero() {
		bt = time.Now()
	}
	ix := &Index{
		bucket:    opts.Bucket,
		prefix:    normalizePrefix(opts.Prefix),
		execMode:  opts.Exec,
		buildTime: bt.UnixNano(),
	}

	// 1. Sanitize.
	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !sanitize(e.Key) {
			ix.dropped++
			continue
		}
		kept = append(kept, e)
	}
	if ix.dropped > 0 && opts.Logger != nil {
		opts.Logger.Warn("dropped invalid keys", "count", ix.dropped)
	}

	// 2. Sort by key, then collapse exact duplicates (last write wins).
	sort.Slice(kept, func(i, j int) bool { return kept[i].Key < kept[j].Key })
	if len(kept) > 0 {
		w := 0
		for r := 1; r < len(kept); r++ {
			if kept[r].Key == kept[w].Key {
				kept[w] = kept[r]
				continue
			}
			w++
			kept[w] = kept[r]
		}
		kept = kept[:w+1]
	}

	// 3. Drop file keys shadowed by a same-named directory: a non-marker key
	// K immediately followed by a key with prefix "K/" is inaccessible.
	final := make([]Entry, 0, len(kept))
	for i := 0; i < len(kept); i++ {
		k := kept[i].Key
		if !strings.HasSuffix(k, "/") && i+1 < len(kept) && strings.HasPrefix(kept[i+1].Key, k+"/") {
			ix.shadowed++
			continue
		}
		final = append(final, kept[i])
	}
	if ix.shadowed > 0 && opts.Logger != nil {
		opts.Logger.Warn("dropped keys shadowed by a same-named directory", "count", ix.shadowed)
	}

	// 4. Pack the file arena and metadata; assign file inodes.
	used := map[uint64]struct{}{0: {}, rootIno: {}}
	n := len(final)
	ix.offs = make([]uint64, n+1)
	ix.sizes = make([]uint64, n)
	ix.mtimes = make([]int64, n)
	ix.etags = make([]uint64, n)
	ix.inos = make([]uint64, n)

	// dirMax accumulates each directory's mtime as the max over its descendants.
	dirMax := map[string]int64{"": 0}

	var arena []byte
	for i, e := range final {
		ix.offs[i] = uint64(len(arena))
		arena = append(arena, e.Key...)
		ix.sizes[i] = uint64(e.Size)
		ix.mtimes[i] = e.MTime
		ix.etags[i] = e.ETagHash
		ix.inos[i] = ix.assignIno(hashKey(ix.prefix+e.Key), used)
		accumulateDirs(dirMax, e.Key, e.MTime)
	}
	ix.offs[n] = uint64(len(arena))
	ix.arena = arena

	// 5. Build the directory table: sorted paths, shared-namespace inodes, and
	// resolved mtimes (max descendant, or build time when none is dated).
	ix.packDirs(dirMax, used)

	if ix.collisions > 0 && opts.Logger != nil {
		opts.Logger.Warn("resolved inode hash collisions", "count", ix.collisions)
	}
	if opts.Logger != nil {
		opts.Logger.Info("index built",
			"keys", n, "dirs", ix.dirCount(),
			"dropped", ix.dropped, "shadowed", ix.shadowed, "collisions", ix.collisions)
	}
	return ix
}

// accumulateDirs updates dirMax for every directory that is an ancestor of
// relKey (and, for a folder marker, the directory the marker itself names),
// setting each to the max mtime seen.
func accumulateDirs(dirMax map[string]int64, relKey string, mtime int64) {
	bump := func(dir string) {
		// Register the directory on first sight (even with a zero mtime), then
		// keep the running max over its descendants.
		if cur, ok := dirMax[dir]; !ok || mtime > cur {
			dirMax[dir] = mtime
		}
	}
	bump("") // root
	s := strings.TrimSuffix(relKey, "/")
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			bump(s[:i])
		}
	}
	if strings.HasSuffix(relKey, "/") {
		// The marker names directory s; it has no deeper descendant here.
		bump(s)
	}
}

// packDirs sorts the accumulated directories, assigns each an inode from the
// shared namespace (root forced to rootIno), resolves mtimes, and packs the
// directory arena and parallel arrays into ix.
func (ix *Index) packDirs(dirMax map[string]int64, used map[uint64]struct{}) {
	dirs := make([]string, 0, len(dirMax))
	for d := range dirMax {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	m := len(dirs)
	ix.dirOffs = make([]uint64, m+1)
	ix.dirMtimes = make([]int64, m)
	ix.dirInos = make([]uint64, m)
	var arena []byte
	for i, d := range dirs {
		ix.dirOffs[i] = uint64(len(arena))
		arena = append(arena, d...)
		mt := dirMax[d]
		if mt == 0 {
			mt = ix.buildTime
		}
		ix.dirMtimes[i] = mt
		if d == "" {
			ix.dirInos[i] = rootIno
			continue
		}
		ix.dirInos[i] = ix.assignIno(hashKey(ix.prefix+d+"/"), used)
	}
	ix.dirOffs[m] = uint64(len(arena))
	ix.dirArena = arena
}

// assignIno returns a unique inode given a base hash, probing forward on
// collision. used tracks every inode already handed out (files and dirs share
// this one namespace).
func (ix *Index) assignIno(base uint64, used map[uint64]struct{}) uint64 {
	h := base
	if _, taken := used[h]; !taken && h != 0 && h != rootIno {
		used[h] = struct{}{}
		return h
	}
	ix.collisions++
	for {
		h++
		if h == 0 || h == rootIno {
			continue
		}
		if _, taken := used[h]; !taken {
			used[h] = struct{}{}
			return h
		}
	}
}

// normalizePrefix trims a leading slash and ensures a non-empty prefix ends
// with exactly one "/".
func normalizePrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(p, "/") + "/"
}
