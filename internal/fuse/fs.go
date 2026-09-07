// SPDX-License-Identifier: Apache-2.0

// Package fuse implements lith's read-only FUSE layer on the hanwen/go-fuse/v2
// raw API. Lookups, attributes, and directory listings are served from the
// Index; file reads go through the BlockStore. Every mutating operation
// returns EROFS. See the pinned Design issue, §4.4.
package fuse

import (
	"context"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
)

// oneYear is used for attribute and entry cache timeouts: the index is
// authoritative and content is immutable, so the kernel can cache indefinitely.
const oneYear = 365 * 24 * time.Hour

// Config wires the FUSE filesystem to its data sources and mount options.
type Config struct {
	Index        *index.Index
	Store        *blockstore.BlockStore
	Metrics      *metrics.Metrics
	UID          uint32
	GID          uint32
	SmallFile    int64 // whole-file prefetch threshold in bytes
	MaxReadahead int64 // max sequential readahead window in blocks
}

// node is an entry in the NodeId table.
type node struct {
	path   string // relative path, "" for root
	parent uint64
	isDir  bool
}

// fileHandle carries per-open-file state, including its prefetcher.
type fileHandle struct {
	key       blockstore.Key
	size      int64
	pf        *pfWrapper
	smallDone bool
	smallMu   sync.Mutex
}

type rawFS struct {
	fuse.RawFileSystem // default (ENOSYS) for anything not overridden

	cfg       Config
	ix        *index.Index
	store     *blockstore.BlockStore
	met       *metrics.Metrics
	blockSize int64
	ctx       context.Context

	mu      sync.RWMutex
	nodes   map[uint64]*node
	handles map[uint64]*fileHandle
	nextFh  uint64
}

// NewRawFileSystem builds the read-only RawFileSystem.
func NewRawFileSystem(cfg Config) fuse.RawFileSystem {
	f := &rawFS{
		RawFileSystem: fuse.NewDefaultRawFileSystem(),
		cfg:           cfg,
		ix:            cfg.Index,
		store:         cfg.Store,
		met:           cfg.Metrics,
		blockSize:     cfg.Store.BlockSize(),
		ctx:           context.Background(),
		nodes:         map[uint64]*node{fuse.FUSE_ROOT_ID: {path: "", parent: fuse.FUSE_ROOT_ID, isDir: true}},
		handles:       map[uint64]*fileHandle{},
		nextFh:        1,
	}
	return f
}

func (f *rawFS) observe(op string, start time.Time) {
	f.met.ObserveFUSE(op, time.Since(start).Seconds())
}

// resolve returns the node for a NodeId.
func (f *rawFS) resolve(id uint64) (*node, bool) {
	f.mu.RLock()
	n, ok := f.nodes[id]
	f.mu.RUnlock()
	return n, ok
}

// register records a NodeId -> path mapping (idempotent).
func (f *rawFS) register(ino uint64, path string, parent uint64, isDir bool) {
	f.mu.Lock()
	if _, ok := f.nodes[ino]; !ok {
		f.nodes[ino] = &node{path: path, parent: parent, isDir: isDir}
	}
	f.mu.Unlock()
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// fillAttr populates a fuse.Attr from an index FileInfo.
func (f *rawFS) fillAttr(a *fuse.Attr, fi index.FileInfo) {
	a.Ino = fi.Ino
	a.Size = uint64(fi.Size)
	a.Blocks = (uint64(fi.Size) + 511) / 512
	mode := uint32(fi.Mode.Perm())
	if fi.IsDir {
		mode |= syscall.S_IFDIR
	} else {
		mode |= syscall.S_IFREG
	}
	a.Mode = mode
	a.Nlink = fi.Nlink
	secs := uint64(fi.MTime.Unix())
	nsec := uint32(fi.MTime.Nanosecond())
	a.Atime, a.Mtime, a.Ctime = secs, secs, secs
	a.Atimensec, a.Mtimensec, a.Ctimensec = nsec, nsec, nsec
	a.Owner.Uid = f.cfg.UID
	a.Owner.Gid = f.cfg.GID
}

// Lookup resolves a child name under a directory NodeId.
func (f *rawFS) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	defer f.observe("lookup", time.Now())
	parent, ok := f.resolve(header.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	childPath := joinPath(parent.path, name)
	fi, err := f.ix.Stat("/" + childPath)
	if err != nil {
		return fuse.ENOENT
	}
	f.register(fi.Ino, childPath, header.NodeId, fi.IsDir)
	out.NodeId = fi.Ino
	out.Generation = 1
	out.SetEntryTimeout(oneYear)
	out.SetAttrTimeout(oneYear)
	f.fillAttr(&out.Attr, fi)
	return fuse.OK
}

// GetAttr returns attributes for a NodeId.
func (f *rawFS) GetAttr(cancel <-chan struct{}, input *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	defer f.observe("getattr", time.Now())
	n, ok := f.resolve(input.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	fi, err := f.ix.Stat("/" + n.path)
	if err != nil {
		return fuse.ENOENT
	}
	out.SetTimeout(oneYear)
	f.fillAttr(&out.Attr, fi)
	return fuse.OK
}

// Open opens a file for reading and allocates a handle with a prefetcher.
func (f *rawFS) Open(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	n, ok := f.resolve(input.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	fi, err := f.ix.Stat("/" + n.path)
	if err != nil {
		return fuse.ENOENT
	}
	if fi.IsDir {
		return fuse.Status(syscall.EISDIR)
	}
	// Reject any write intent.
	if input.Flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_CREAT|syscall.O_TRUNC) != 0 {
		return fuse.Status(syscall.EROFS)
	}
	h := &fileHandle{
		key:  blockstore.Key{Key: f.objectKey(n.path), ETagHash: f.ix.ETagHashOf("/" + n.path)},
		size: fi.Size,
		pf:   newPFWrapper(f.maxReadahead()),
	}
	f.mu.Lock()
	fh := f.nextFh
	f.nextFh++
	f.handles[fh] = h
	f.mu.Unlock()

	// Dispatch the initial readahead window at open (before the first read)
	// for files worth prefetching, so the frontier leads from the start (#38).
	if fi.Size > f.cfg.SmallFile {
		for _, pb := range h.pf.open() {
			pb := pb
			go f.store.Prefetch(f.ctx, h.key, pb, h.size)
		}
	}

	out.Fh = fh
	out.OpenFlags = fuse.FOPEN_KEEP_CACHE // content is immutable
	return fuse.OK
}

// Read serves a read from the block store and drives prefetch.
func (f *rawFS) Read(cancel <-chan struct{}, input *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	start := time.Now()
	defer f.observe("read", start)
	f.mu.RLock()
	h := f.handles[input.Fh]
	f.mu.RUnlock()
	if h == nil {
		return nil, fuse.EBADF
	}

	f.maybeWholeFile(h)

	off := int64(input.Offset)
	length := int64(len(buf))
	end := off + length
	if end > h.size {
		end = h.size
	}

	var res fuse.ReadResult
	if off < end && off/blockstore.ChunkSize == (end-1)/blockstore.ChunkSize {
		// Read lies within one chunk: return a sub-slice of the (immutable)
		// chunk buffer directly — no copy, no allocation on a cache hit.
		ci := off / blockstore.ChunkSize
		chunk, err := f.store.Chunk(f.ctx, h.key, ci, h.size)
		if err != nil {
			return nil, fuse.EIO
		}
		lo := off - ci*blockstore.ChunkSize
		hi := lo + (end - off)
		if hi > int64(len(chunk)) {
			hi = int64(len(chunk))
		}
		res = fuse.ReadResultData(chunk[lo:hi])
	} else {
		// Straddles a chunk boundary: assemble (a copy).
		f.cfg.Metrics.ReadStraddle()
		data, err := f.store.GetRange(f.ctx, h.key, off, length, h.size)
		if err != nil {
			return nil, fuse.EIO
		}
		res = fuse.ReadResultData(data)
	}

	// Drive the prefetcher off the block this read falls in.
	blk := off / f.blockSize
	for _, pb := range h.pf.observe(blk) {
		pb := pb
		go f.store.Prefetch(f.ctx, h.key, pb, h.size)
	}
	return res, fuse.OK
}

// maybeWholeFile fetches an entire small file on first read.
func (f *rawFS) maybeWholeFile(h *fileHandle) {
	if f.cfg.SmallFile <= 0 || h.size > f.cfg.SmallFile {
		return
	}
	h.smallMu.Lock()
	defer h.smallMu.Unlock()
	if h.smallDone {
		return
	}
	h.smallDone = true
	nb := (h.size + f.blockSize - 1) / f.blockSize
	for b := int64(0); b < nb; b++ {
		go f.store.Prefetch(f.ctx, h.key, b, h.size)
	}
}

// Release frees a file handle.
func (f *rawFS) Release(cancel <-chan struct{}, input *fuse.ReleaseIn) {
	f.mu.Lock()
	delete(f.handles, input.Fh)
	f.mu.Unlock()
}

// OpenDir accepts a directory open (state is carried via the read offset).
func (f *rawFS) OpenDir(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	n, ok := f.resolve(input.NodeId)
	if !ok || !n.isDir {
		return fuse.ENOTDIR
	}
	return fuse.OK
}

// ReadDir lists directory entries (names + inode + type). Offsets 1 and 2 are
// "." and ".."; real entries carry offset = indexCursor+2.
func (f *rawFS) ReadDir(cancel <-chan struct{}, input *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	defer f.observe("readdir", time.Now())
	return f.readdir(input, out, false)
}

// ReadDirPlus lists entries and returns their attributes, registering a node
// per entry (a lookup).
func (f *rawFS) ReadDirPlus(cancel <-chan struct{}, input *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	defer f.observe("readdirplus", time.Now())
	return f.readdir(input, out, true)
}

func (f *rawFS) readdir(input *fuse.ReadIn, out *fuse.DirEntryList, plus bool) fuse.Status {
	n, ok := f.resolve(input.NodeId)
	if !ok || !n.isDir {
		return fuse.ENOTDIR
	}
	self, err := f.ix.Stat("/" + n.path)
	if err != nil {
		return fuse.ENOENT
	}
	cursor := input.Offset

	// "." and ".." only in the non-plus listing (offsets 1 and 2).
	if !plus {
		if cursor == 0 {
			if !out.AddDirEntry(fuse.DirEntry{Mode: syscall.S_IFDIR, Name: ".", Ino: self.Ino, Off: 1}) {
				return fuse.OK
			}
			cursor = 1
		}
		if cursor == 1 {
			parentIno := self.Ino
			if pn, ok := f.resolve(n.parent); ok {
				if pfi, e := f.ix.Stat("/" + pn.path); e == nil {
					parentIno = pfi.Ino
				}
			}
			if !out.AddDirEntry(fuse.DirEntry{Mode: syscall.S_IFDIR, Name: "..", Ino: parentIno, Off: 2}) {
				return fuse.OK
			}
			cursor = 2
		}
	} else if cursor == 0 {
		cursor = 2 // keep the same offset base as the non-plus path
	}

	rc := cursor - 2
	for {
		ents, next, err := f.ix.Readdir("/"+n.path, rc, 1)
		if err != nil {
			return fuse.ENOENT
		}
		if len(ents) == 0 {
			break
		}
		e := ents[0]
		mode := uint32(syscall.S_IFREG)
		if e.IsDir {
			mode = syscall.S_IFDIR
		}
		de := fuse.DirEntry{Mode: mode, Name: e.Name, Ino: e.Ino, Off: next + 2}
		if plus {
			eo := out.AddDirLookupEntry(de)
			if eo == nil {
				break
			}
			childPath := joinPath(n.path, e.Name)
			f.register(e.Ino, childPath, input.NodeId, e.IsDir)
			if fi, e2 := f.ix.Stat("/" + childPath); e2 == nil {
				eo.NodeId = fi.Ino
				eo.Generation = 1
				eo.SetEntryTimeout(oneYear)
				eo.SetAttrTimeout(oneYear)
				f.fillAttr(&eo.Attr, fi)
			}
		} else {
			if !out.AddDirEntry(de) {
				break
			}
		}
		rc = next
	}
	return fuse.OK
}

// ReleaseDir is a no-op (no per-dir state is held).
func (f *rawFS) ReleaseDir(input *fuse.ReleaseIn) {}

// StatFs reports totals derived from the index; the filesystem has no free
// space (it is read-only).
func (f *rawFS) StatFs(cancel <-chan struct{}, input *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	const bsize = 4096
	total := uint64(f.ix.TotalSize())
	out.Bsize = bsize
	out.Frsize = bsize
	out.Blocks = (total + bsize - 1) / bsize
	out.Bfree = 0
	out.Bavail = 0
	out.Files = uint64(f.ix.Len())
	out.Ffree = 0
	out.NameLen = 255
	return fuse.OK
}

func (f *rawFS) objectKey(relPath string) string {
	return f.ix.Prefix() + relPath
}

func (f *rawFS) maxReadahead() int64 {
	if f.cfg.MaxReadahead > 0 {
		return f.cfg.MaxReadahead
	}
	return 32
}
