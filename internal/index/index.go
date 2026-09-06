// SPDX-License-Identifier: Apache-2.0

// Package index holds the immutable namespace snapshot of a bucket: an
// in-memory sorted arena of keys plus parallel metadata arrays, from which
// directory structure is derived rather than stored. See the pinned Design
// issue, §4.1.
package index

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// ErrNotExist is returned by Lookup and Stat for a path that is neither a
// stored object nor a derived directory.
var ErrNotExist = errors.New("index: path does not exist")

// rootIno is the inode number reserved for the mount root directory.
const rootIno uint64 = 1

// FileInfo is the result of Stat: the POSIX attributes lith derives for a path.
type FileInfo struct {
	Ino   uint64
	Size  int64
	MTime time.Time
	Mode  fs.FileMode
	IsDir bool
	Nlink uint32
}

// Dirent is a single entry returned by Readdir.
type Dirent struct {
	Name  string
	Ino   uint64
	IsDir bool
}

// Index is an immutable, queryable namespace snapshot. It is safe for
// concurrent readers; it is never mutated after construction.
type Index struct {
	bucket string
	prefix string // mount root within the bucket; "" or "some/prefix/"

	// arena holds all sorted, relative keys concatenated. offs has len n+1;
	// key i is arena[offs[i]:offs[i+1]].
	arena []byte
	offs  []uint32

	// Parallel per-key metadata (SoA), each of length n.
	sizes  []uint64
	mtimes []int64 // Unix nanoseconds
	etags  []uint64
	inos   []uint64

	// Directory table (format v2): every directory, including the root (""),
	// sorted by relative path. Inodes are drawn from the same collision
	// namespace as file inodes; mtimes are the max LastModified over each
	// directory's descendants (falling back to buildTime).
	dirArena  []byte
	dirOffs   []uint32
	dirMtimes []int64  // Unix nanoseconds
	dirInos   []uint64 // parallel to the dir table
	buildTime int64    // Unix nanoseconds; mtime fallback for empty directories

	execMode bool // when true, files are 0555 instead of 0444

	// Build statistics, surfaced by inspect.
	dropped    uint64 // keys rejected by sanitization
	shadowed   uint64 // file keys shadowed by a same-named directory
	collisions uint64 // inode hash collisions resolved by probing
}

// Bucket returns the bucket the index was built from.
func (ix *Index) Bucket() string { return ix.bucket }

// Prefix returns the mount root within the bucket (may be "").
func (ix *Index) Prefix() string { return ix.prefix }

// Len returns the number of stored keys (files plus folder markers).
func (ix *Index) Len() int { return len(ix.offs) - 1 }

// Stats returns the build statistics recorded when the index was created.
func (ix *Index) Stats() (dropped, shadowed, collisions uint64) {
	return ix.dropped, ix.shadowed, ix.collisions
}

// key returns the relative key at position i.
func (ix *Index) key(i int) string {
	return string(ix.arena[ix.offs[i]:ix.offs[i+1]])
}

// keyBytes returns the relative key at position i without copying.
func (ix *Index) keyBytes(i int) []byte {
	return ix.arena[ix.offs[i]:ix.offs[i+1]]
}

// lowerBound returns the smallest index i such that key(i) >= s.
func (ix *Index) lowerBound(s string) int {
	return sort.Search(ix.Len(), func(i int) bool {
		return string(ix.keyBytes(i)) >= s
	})
}

// dirCount returns the number of directories in the table (includes root).
func (ix *Index) dirCount() int { return len(ix.dirOffs) - 1 }

// dirPath returns the relative directory path at table position i.
func (ix *Index) dirPath(i int) string {
	return string(ix.dirArena[ix.dirOffs[i]:ix.dirOffs[i+1]])
}

// dirLookup finds the directory table entry for the relative path rel ("" is
// root) and returns its inode and mtime.
func (ix *Index) dirLookup(rel string) (ino uint64, mtime int64, ok bool) {
	n := ix.dirCount()
	i := sort.Search(n, func(i int) bool { return ix.dirPath(i) >= rel })
	if i < n && ix.dirPath(i) == rel {
		return ix.dirInos[i], ix.dirMtimes[i], true
	}
	return 0, 0, false
}

// findExact returns the index of an exact key match and whether it was found.
func (ix *Index) findExact(rel string) (int, bool) {
	i := ix.lowerBound(rel)
	if i < ix.Len() && ix.key(i) == rel {
		return i, true
	}
	return 0, false
}

// dirExists reports whether rel names a directory, per the directory table.
func (ix *Index) dirExists(rel string) bool {
	_, _, ok := ix.dirLookup(rel)
	return ok
}

// Lookup resolves a path to its FileInfo. path is slash-rooted relative to the
// mount (e.g. "/a/b"); "" and "/" name the root.
func (ix *Index) Lookup(path string) (FileInfo, error) {
	return ix.Stat(path)
}

// Stat returns the FileInfo for path. Directories take precedence over a
// same-named object (such objects are dropped at build time as shadowed).
func (ix *Index) Stat(path string) (FileInfo, error) {
	rel := toRel(path)
	if rel == "" {
		return ix.dirInfo(""), nil
	}
	if ix.dirExists(rel) {
		return ix.dirInfo(rel), nil
	}
	if i, ok := ix.findExact(rel); ok && !strings.HasSuffix(rel, "/") {
		return ix.fileInfo(i), nil
	}
	return FileInfo{}, ErrNotExist
}

func (ix *Index) fileInfo(i int) FileInfo {
	mode := fs.FileMode(0o444)
	if ix.execMode {
		mode = 0o555
	}
	return FileInfo{
		Ino:   ix.inos[i],
		Size:  int64(ix.sizes[i]),
		MTime: time.Unix(0, ix.mtimes[i]),
		Mode:  mode,
		IsDir: false,
		Nlink: 1,
	}
}

// dirInfo builds the FileInfo for the directory named by rel ("" is root). The
// inode and mtime come from the directory table; if the directory is somehow
// absent from the table (should not happen for a valid path), the inode and
// mtime fall back to the root inode and build time.
func (ix *Index) dirInfo(rel string) FileInfo {
	ino, mtimeNs, ok := ix.dirLookup(rel)
	if !ok {
		ino, mtimeNs = rootIno, ix.buildTime
	}
	// nlink for a directory is 2 + number of immediate subdirectories.
	subdirs := ix.countSubdirs(rel)
	return FileInfo{
		Ino:   ino,
		Size:  0,
		MTime: time.Unix(0, mtimeNs),
		Mode:  fs.ModeDir | 0o555,
		IsDir: true,
		Nlink: 2 + subdirs,
	}
}

// countSubdirs counts the immediate subdirectories of the directory named by
// rel ("" is root).
func (ix *Index) countSubdirs(rel string) uint32 {
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	var n uint32
	i := ix.lowerBound(prefix)
	for i < ix.Len() {
		k := ix.key(i)
		if !strings.HasPrefix(k, prefix) {
			break
		}
		rem := k[len(prefix):]
		if slash := strings.IndexByte(rem, '/'); slash >= 0 {
			n++
			// Skip the rest of this subdirectory's keys.
			i = ix.skipDir(prefix, rem[:slash])
			continue
		}
		i++
	}
	return n
}

// skipDir returns the first index at or after the run of keys under
// prefix+name+"/", i.e. it jumps past every child of that subdirectory.
func (ix *Index) skipDir(prefix, name string) int {
	// The children live in [prefix+name+"/", prefix+name+"0"), because '0'
	// (0x30) is the byte immediately after '/' (0x2f).
	upper := prefix + name + string(byte('/'+1))
	return ix.lowerBound(upper)
}

// Readdir lists the immediate children of the directory named by path.
// cursor is 0 on the first call; pass the returned cursor to continue. An
// empty result slice signals the end of the directory. n must be > 0.
func (ix *Index) Readdir(path string, cursor uint64, n int) ([]Dirent, uint64, error) {
	rel := toRel(path)
	if rel != "" && !ix.dirExists(rel) {
		return nil, 0, ErrNotExist
	}
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	if n <= 0 {
		n = 1 << 30
	}

	var start int
	if cursor == 0 {
		start = ix.lowerBound(prefix)
	} else {
		start = int(cursor - 1)
	}

	out := make([]Dirent, 0, min(n, 256))
	i := start
	for i < ix.Len() && len(out) < n {
		k := ix.key(i)
		if !strings.HasPrefix(k, prefix) {
			i = ix.Len()
			break
		}
		rem := k[len(prefix):]
		if rem == "" {
			// The directory's own folder-marker key ("prefix/"); not a child.
			i++
			continue
		}
		if slash := strings.IndexByte(rem, '/'); slash >= 0 {
			name := rem[:slash]
			childIno, _, _ := ix.dirLookup(prefix + name)
			out = append(out, Dirent{
				Name:  name,
				Ino:   childIno,
				IsDir: true,
			})
			i = ix.skipDir(prefix, name)
			continue
		}
		// A file child.
		out = append(out, Dirent{Name: rem, Ino: ix.inos[i], IsDir: false})
		i++
	}
	return out, uint64(i + 1), nil
}

// toRel converts a slash-rooted mount path to a relative key: it trims a
// single leading slash. "" and "/" both map to "" (the root).
func toRel(path string) string {
	return strings.TrimPrefix(path, "/")
}
