// SPDX-License-Identifier: Apache-2.0

package index

import (
	"sort"
	"strings"
)

// inoDirBit marks an inoRefs entry as a directory-table position (rather than a
// key position). Positions are well under 2^31 for any realistic index.
const inoDirBit uint32 = 1 << 31

// buildInoIndex builds the sorted inode -> (path position) lookup used by
// ByInode. Files and directories share one inode namespace (assignIno probes
// forward so every inode is unique), so one sorted array covers both. Built
// once, lazily, on the first ByInode call — the cost is ~12 bytes/entry plus a
// one-time sort; measured under 200 ms even on a 10M-key index (session 39).
func (ix *Index) buildInoIndex() {
	type rec struct {
		ino uint64
		ref uint32
	}
	recs := make([]rec, 0, len(ix.inos)+ix.dirCount())
	for i, ino := range ix.inos {
		recs = append(recs, rec{ino, uint32(i)})
	}
	for i, ino := range ix.dirInos {
		recs = append(recs, rec{ino, uint32(i) | inoDirBit})
	}
	sort.Slice(recs, func(a, b int) bool { return recs[a].ino < recs[b].ino })
	ix.inoKeys = make([]uint64, len(recs))
	ix.inoRefs = make([]uint32, len(recs))
	for i, r := range recs {
		ix.inoKeys[i] = r.ino
		ix.inoRefs[i] = r.ref
	}
}

// ByInode returns the "/"-rooted path for an inode, or ok=false if no entry has
// that inode. It is the reverse of the inode assigned at build time, so an NFS
// file handle carrying an inode resolves back to a path across gateway restarts
// against the same index (#144). O(log n) after a one-time build.
func (ix *Index) ByInode(ino uint64) (string, bool) {
	ix.inoOnce.Do(ix.buildInoIndex)
	j := sort.Search(len(ix.inoKeys), func(i int) bool { return ix.inoKeys[i] >= ino })
	if j >= len(ix.inoKeys) || ix.inoKeys[j] != ino {
		return "", false
	}
	ref := ix.inoRefs[j]
	if ref&inoDirBit != 0 {
		d := ix.dirPath(int(ref &^ inoDirBit)) // "" for root
		return "/" + d, true
	}
	return "/" + ix.key(int(ref)), true
}

// ByInode resolves an inode within a sub-root view: the path must live under the
// view's root, and is returned view-relative ("/"-rooted). An inode outside the
// view's range is not found.
func (v *View) ByInode(ino uint64) (string, bool) {
	p, ok := v.ix.ByInode(ino)
	if !ok {
		return "", false
	}
	if v.sub == "" {
		return p, true
	}
	rel := toRel(p) // index-relative, no leading slash
	if rel == strings.TrimSuffix(v.sub, "/") {
		return "/", true // the view root directory itself
	}
	if strings.HasPrefix(rel, v.sub) { // v.sub ends in "/"
		return "/" + strings.TrimPrefix(rel, v.sub), true
	}
	return "", false // inode is outside this view
}
