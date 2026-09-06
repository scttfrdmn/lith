// SPDX-License-Identifier: Apache-2.0

package index

import (
	"log/slog"
	"sort"
	"strings"

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
	// Logger receives build progress and warnings; nil disables logging.
	Logger *slog.Logger
}

// HashETag returns the 64-bit hash lith stores for an ETag string.
func HashETag(etag string) uint64 { return xxh3.HashString(etag) }

// dirIno returns the synthetic inode number for a derived directory, hashed
// from its full (prefix-qualified) path ending in "/".
func dirIno(full string) uint64 {
	h := xxh3.HashString(full)
	// Never collide with the reserved root inode or the invalid 0.
	if h == 0 || h == rootIno {
		h += 2
	}
	return h
}

// sanitize reports whether a relative key is a valid POSIX path. A single
// trailing "/" (a folder marker) is allowed; embedded "//", "." or ".."
// components, a NUL byte, or an empty key are rejected.
func sanitize(rel string) bool {
	if rel == "" {
		return false
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return false
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
// directory, assigns inodes (xxh3 with collision fallback), and packs the
// sorted keys into the arena with parallel metadata arrays.
func Build(entries []Entry, opts Options) *Index {
	ix := &Index{
		bucket:   opts.Bucket,
		prefix:   normalizePrefix(opts.Prefix),
		execMode: opts.Exec,
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

	// 4. Pack the arena and parallel metadata; assign inodes.
	n := len(final)
	ix.offs = make([]uint32, n+1)
	ix.sizes = make([]uint64, n)
	ix.mtimes = make([]int64, n)
	ix.etags = make([]uint64, n)
	ix.inos = make([]uint64, n)

	used := map[uint64]struct{}{0: {}, rootIno: {}}
	var arena []byte
	for i, e := range final {
		ix.offs[i] = uint32(len(arena))
		arena = append(arena, e.Key...)
		ix.sizes[i] = uint64(e.Size)
		ix.mtimes[i] = e.MTime
		ix.etags[i] = e.ETagHash
		ix.inos[i] = ix.assignIno(e.Key, used)
	}
	ix.offs[n] = uint32(len(arena))
	ix.arena = arena

	if ix.collisions > 0 && opts.Logger != nil {
		opts.Logger.Warn("resolved inode hash collisions", "count", ix.collisions)
	}
	if opts.Logger != nil {
		opts.Logger.Info("index built",
			"keys", n, "dropped", ix.dropped, "shadowed", ix.shadowed, "collisions", ix.collisions)
	}
	return ix
}

// assignIno assigns a unique inode for key: xxh3 of the full key, probing
// forward on collision. used tracks already-assigned values.
func (ix *Index) assignIno(relKey string, used map[uint64]struct{}) uint64 {
	h := xxh3.HashString(ix.prefix + relKey)
	if _, taken := used[h]; !taken {
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
