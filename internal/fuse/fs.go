// SPDX-License-Identifier: Apache-2.0

// Package fuse implements lith's read-only FUSE layer on the hanwen/go-fuse/v2
// raw API. Lookups, attributes, and directory listings are served from the
// Index; file reads go through the BlockStore. Every mutating operation
// returns EROFS. See the pinned Design issue, §4.4.
package fuse

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
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
	PartsMax     int64 // largest file fetched whole as parallel parts on first read (#69); 0 falls back to SmallFile
	MaxReadahead int64 // max sequential readahead window in blocks
	// Limits is the single prefetch policy object (#64): the per-handle
	// readahead window, sibling readahead (#63), and small-file parts (#69) all
	// query it for budget and neighborhood. When nil, the FUSE layer falls back
	// to the store's budget directly (behavior-preserving for callers that do
	// not build a policy, e.g. older tests).
	Limits prefetch.Limits
	// SiblingWindow is the maximum index-position gap between successive opens in
	// one directory that still counts as walking it in key order (#63); <=0 uses
	// the default of 4.
	SiblingWindow int
	// SiblingReadahead is how many following siblings a detected directory walk
	// prefetches whole (#63); 0 disables sibling readahead.
	SiblingReadahead int
	// PrefetchStats, when non-nil, collects per-handle prefetcher diagnostics
	// at Release (the #49 investigation). nil in production.
	PrefetchStats *PrefetchStats
	// SiblingStats, when non-nil, collects sibling-readahead accuracy counters
	// (the #63 guardrail). nil is fine (metrics still record when configured).
	SiblingStats *SiblingStats
}

// SiblingStats aggregates sibling-readahead accuracy across a run (#63).
type SiblingStats struct {
	Prefetched atomic.Int64 // sibling objects dispatched
	Used       atomic.Int64 // sibling-prefetched objects later opened
	Unread     atomic.Int64 // sibling-prefetched objects that fell out of the pending window unopened
}

// PrefetchStats aggregates per-handle prefetcher behaviour across a run.
type PrefetchStats struct {
	mu          sync.Mutex
	Halvings    int64   // total window halvings (seeks) across all handles
	Resets      int64   // total collapses to Random across all handles
	PeakWindows []int64 // each handle's largest readahead window
}

func (p *PrefetchStats) record(halvings, resets, peak int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.Halvings += halvings
	p.Resets += resets
	p.PeakWindows = append(p.PeakWindows, peak)
	p.mu.Unlock()
}

func (s *SiblingStats) recordPrefetched() {
	if s != nil {
		s.Prefetched.Add(1)
	}
}

func (s *SiblingStats) recordUsed() {
	if s != nil {
		s.Used.Add(1)
	}
}

func (s *SiblingStats) recordUnread() {
	if s != nil {
		s.Unread.Add(1)
	}
}

// Snapshot returns the total halving and reset counts and a copy of the
// per-handle peak windows recorded so far.
func (p *PrefetchStats) Snapshot() (halvings, resets int64, peaks []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Halvings, p.Resets, append([]int64(nil), p.PeakWindows...)
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
	// partsDispatched is set once a whole-file parts fetch (#69) has been
	// dispatched for this handle. While set, Read skips its per-handle readahead:
	// the parts fetch already covers every block, so readahead would only
	// contend with it for the same chunks (see Open).
	partsDispatched atomic.Bool
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

	// Sibling-readahead state (#63), guarded by sibMu.
	sibMu         sync.Mutex
	sibLastPos    map[string]int      // directory -> index position of its last open
	sibPendingSet map[string]struct{} // sibling-prefetched keys not yet opened
	sibPendingQ   []string            // FIFO of the same keys, bounded by sibPendingCap
	sibPendingCap int
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
		sibLastPos:    map[string]int{},
		sibPendingSet: map[string]struct{}{},
	}
	// Bound the pending-sibling tracker to a few readahead batches: a key that
	// falls out of it without being opened is counted as an unread sibling.
	f.sibPendingCap = cfg.SiblingReadahead * 4
	if f.sibPendingCap < 64 {
		f.sibPendingCap = 64
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

	// Dispatch the initial readahead window at open so the frontier leads from
	// the start (#38) — but only for files larger than the parts threshold. A
	// file at or below it is fetched whole as parallel parts on first read (#69),
	// which already covers every block, so per-handle readahead is both redundant
	// and a source of contention: the window and the parts fetch would each issue
	// a Prefetch for the same low blocks. That contention is *correct* — the chunk
	// singleflight (session 3) makes concurrent claims for one chunk join a single
	// fetch and never double-read it — but an unlucky interleave can split a
	// would-be single coalesced range GET into two (one goroutine ends up owning
	// the head chunks, the other the tail), costing an extra GET, not wrong data.
	// Skipping the window here, and the read-driven readahead in Read for the same
	// handle (via partsDispatched), removes the contention for parts-covered files
	// outright; a budget-exhausted parts fetch leaves partsDispatched false, so
	// readahead still serves those files.
	if fi.Size > f.partsThreshold() {
		for _, pb := range h.pf.open(f.perHandleWindow()) {
			pb := pb
			go f.store.Prefetch(f.ctx, h.key, pb, h.size)
		}
	}

	// If this directory is being walked in key order, read ahead across the
	// following siblings (#63).
	f.maybeSiblingReadahead(n.path)

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

	f.maybePartsFetch(h)

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

	// Drive the prefetcher off the block this read falls in — unless a whole-file
	// parts fetch is in flight for this handle, which already covers every block
	// (see Open: readahead would only contend with the parts fetch).
	if !h.partsDispatched.Load() {
		blk := off / f.blockSize
		for _, pb := range h.pf.observe(blk, f.perHandleWindow()) {
			pb := pb
			go f.store.Prefetch(f.ctx, h.key, pb, h.size)
		}
	}
	return res, fuse.OK
}

// maybePartsFetch, on the first read of a file small enough to fetch whole
// (<= the parts threshold), fetches every block concurrently as parallel range
// parts through the normal fill path rather than letting the app pull the file
// one chunk at a time. This is the #69 fix: a mid-size object (the 31 MB HDF5
// granule) that a copy tool parallelizes into range parts and reads faster than
// lith's serial per-chunk demand fetches; a file spanning N blocks becomes N
// concurrent block-sized (part-sized) range GETs, a single GET when it fits in
// one part. The eager fetch is budget-bounded via Limits (#64): many handles
// opening mid-size files at once cannot exceed the mount-wide prefetch budget,
// and when it is full the eager fetch is skipped so per-read readahead still
// serves the file.
func (f *rawFS) maybePartsFetch(h *fileHandle) {
	threshold := f.partsThreshold()
	if threshold <= 0 || h.size > threshold {
		return
	}
	h.smallMu.Lock()
	if h.smallDone {
		h.smallMu.Unlock()
		return
	}
	h.smallDone = true
	h.smallMu.Unlock()

	// Fetch the whole file as parallel parts, budget-permitting; on a full
	// budget per-read readahead still serves the file. When it is dispatched,
	// mark the handle so Read skips its redundant per-handle readahead (which
	// would only contend with the parts fetch for the same chunks; see Open).
	if f.prefetchWhole(h.key, h.size) {
		h.partsDispatched.Store(true)
	}
}

// prefetchWhole reserves the object's size against the prefetch budget (#64)
// and, if it fits, fetches every block concurrently as parallel range parts
// (#69), releasing the reservation when they complete. It returns false when
// the budget is full so a caller issuing several prefetches (sibling readahead)
// stops before starving demand.
func (f *rawFS) prefetchWhole(key blockstore.Key, size int64) bool {
	if size <= 0 {
		return true
	}
	if !f.reserve(size) {
		return false
	}
	nb := (size + f.blockSize - 1) / f.blockSize
	go func() {
		defer f.release(size)
		var wg sync.WaitGroup
		for b := int64(0); b < nb; b++ {
			wg.Add(1)
			go func(b int64) {
				defer wg.Done()
				f.store.Prefetch(f.ctx, key, b, size)
			}(b)
		}
		wg.Wait()
	}()
	return true
}

// maybeSiblingReadahead, on the open of a file whose index position is within
// --sibling-window of the previous open in the same directory, treats the
// directory as being walked in key order and prefetches the next
// --sibling-readahead siblings whole (each if it is at or below --small-file),
// via the parts path (#69), bounded by the prefetch budget (#64). Chunked
// stores (Zarr, sharded datasets) are read as many small sibling objects in key
// order, and the per-file prefetcher never sees the next object; this closes
// that gap (#63). Budget exhaustion stops the loop so handle readahead is not
// starved. A first open outside the window resets the walk.
func (f *rawFS) maybeSiblingReadahead(relPath string) {
	if f.cfg.SiblingReadahead <= 0 || f.cfg.Limits == nil {
		return
	}
	pos, ok := f.ix.Position("/" + relPath)
	if !ok {
		return
	}
	dir := dirOf(relPath)

	f.sibMu.Lock()
	prev, seen := f.sibLastPos[dir]
	f.sibLastPos[dir] = pos
	f.markSiblingUsedLocked(relPath) // this file paid off an earlier sibling prefetch
	f.sibMu.Unlock()

	window := f.cfg.SiblingWindow
	if window <= 0 {
		window = 4
	}
	// A forward step within the window is a walk; anything else (a backward
	// step, a jump beyond the window, or the first open) is not.
	if !seen || pos <= prev || pos-prev > window {
		return
	}

	sibs := f.cfg.Limits.Neighborhood("/"+relPath, f.cfg.SiblingReadahead)
	for _, s := range sibs {
		if s.Size <= 0 || s.Size > f.cfg.SmallFile {
			continue // only whole-fetch genuinely small siblings
		}
		key := blockstore.Key{Key: f.ix.Prefix() + s.Key, ETagHash: s.ETagHash}
		if !f.prefetchWhole(key, s.Size) {
			break // budget exhausted: stop before starving handle readahead
		}
		f.noteSiblingIssued(s.Key)
	}
}

// noteSiblingIssued records that key was sibling-prefetched, counting it, and
// evicts the oldest pending sibling if the bounded tracker overflows — a key
// that falls out without being opened is an unread sibling (the #63 guardrail).
func (f *rawFS) noteSiblingIssued(key string) {
	f.sibMu.Lock()
	defer f.sibMu.Unlock()
	if _, dup := f.sibPendingSet[key]; dup {
		return
	}
	f.sibPendingSet[key] = struct{}{}
	f.sibPendingQ = append(f.sibPendingQ, key)
	f.cfg.SiblingStats.recordPrefetched()
	f.met.SiblingPrefetch(1)
	for len(f.sibPendingQ) > f.sibPendingCap {
		old := f.sibPendingQ[0]
		f.sibPendingQ = f.sibPendingQ[1:]
		if _, still := f.sibPendingSet[old]; still {
			delete(f.sibPendingSet, old)
			f.cfg.SiblingStats.recordUnread()
			f.met.SiblingPrefetchUnread(1)
		}
	}
}

// markSiblingUsedLocked clears key from the pending tracker (it was opened, so
// the sibling prefetch paid off). Caller holds sibMu.
func (f *rawFS) markSiblingUsedLocked(key string) {
	if _, ok := f.sibPendingSet[key]; ok {
		delete(f.sibPendingSet, key)
		f.cfg.SiblingStats.recordUsed()
	}
}

// dirOf returns the directory portion of a relative key ("" for a root-level
// file).
func dirOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// partsThreshold is the largest file eagerly fetched whole on first read: the
// #69 parts threshold when set, else the legacy small-file threshold.
func (f *rawFS) partsThreshold() int64 {
	if f.cfg.PartsMax > 0 {
		return f.cfg.PartsMax
	}
	return f.cfg.SmallFile
}

// reserve reserves n bytes of prefetch budget through the Limits policy (#64);
// with no policy configured it always succeeds and reserves nothing.
func (f *rawFS) reserve(n int64) bool {
	if f.cfg.Limits == nil {
		return true
	}
	return f.cfg.Limits.Reserve(n)
}

// release returns n bytes of prefetch budget (no-op with no policy).
func (f *rawFS) release(n int64) {
	if f.cfg.Limits != nil {
		f.cfg.Limits.Release(n)
	}
}

// Release frees a file handle.
func (f *rawFS) Release(cancel <-chan struct{}, input *fuse.ReleaseIn) {
	f.mu.Lock()
	h := f.handles[input.Fh]
	delete(f.handles, input.Fh)
	f.mu.Unlock()
	if h != nil {
		halvings, resets := h.pf.halvings(), h.pf.resets()
		f.cfg.PrefetchStats.record(halvings, resets, h.pf.peakWindow())
		f.cfg.Metrics.PrefetchSeeks(halvings, resets)
	}
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

// perHandleWindow is the readahead window (blocks) each open handle may use so
// their windows share the prefetch budget: budgetBlocks / openHandles, floored
// at 2 and capped by the configured --max-readahead. With one handle it returns
// the full configured window; with many, a fair share that keeps aggregate
// readahead within the memory tier (#55).
func (f *rawFS) perHandleWindow() int64 {
	maxW := f.maxReadahead()
	budgetBlocks := f.budgetBlocks()
	if budgetBlocks <= 0 {
		return maxW
	}
	f.mu.RLock()
	n := int64(len(f.handles))
	f.mu.RUnlock()
	if n < 1 {
		n = 1
	}
	share := budgetBlocks / n
	if share < 2 {
		share = 2
	}
	if share > maxW {
		share = maxW
	}
	return share
}

// budgetBlocks is the mount-wide prefetch budget expressed in readahead blocks.
// The value comes from the Limits policy (#64) when one is configured — the
// single source for "how much un-demanded prefetch may be outstanding" — and
// falls back to the store's budget otherwise. Both yield the same number; the
// policy is the seam through which sibling readahead and parts share the budget.
func (f *rawFS) budgetBlocks() int64 {
	if f.cfg.Limits != nil {
		total, _ := f.cfg.Limits.Budget()
		if bs := f.store.BlockSize(); bs > 0 && total > 0 {
			if n := total / bs; n >= 1 {
				return n
			}
			return 1
		}
		return 0
	}
	return f.store.PrefetchBudgetBlocks()
}
