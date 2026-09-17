// SPDX-License-Identifier: Apache-2.0

// Package fuse implements lith's read-only FUSE layer on the hanwen/go-fuse/v2
// raw API. Lookups, attributes, and directory listings are served from the
// Index; file reads go through the BlockStore. Every mutating operation
// returns EROFS. See the pinned Design issue, §4.4.
package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/format/bgzf"
	"github.com/scttfrdmn/lith/internal/format/footer"
	"github.com/scttfrdmn/lith/internal/index"
	"github.com/scttfrdmn/lith/internal/metrics"
	"github.com/scttfrdmn/lith/internal/prefetch"
)

// oneYear is used for attribute and entry cache timeouts: the index is
// authoritative and content is immutable, so the kernel can cache indefinitely.
const oneYear = 365 * 24 * time.Hour

// Config wires the FUSE filesystem to its data sources and mount options.
type Config struct {
	Index        index.Reader
	Store        *blockstore.BlockStore
	Metrics      *metrics.Metrics
	UID          uint32
	GID          uint32
	SmallFile    int64 // whole-file prefetch threshold in bytes
	PartsMax     int64 // largest file fetched whole as parallel parts on first read (#69); 0 falls back to SmallFile
	MaxReadahead int64 // max sequential readahead window in blocks
	// BgzfWholeFileMax is the largest bgzf data file (with an index sibling)
	// prefetched whole on open (#107); above it, tier-2 slice ranges are used.
	// 0 uses the default of 512 MiB.
	BgzfWholeFileMax int64
	// DisableFooterTier2 turns off the footer family's tier-2 projection/entry
	// prefetch (#108), leaving only the generic tier-1 footer+head prefetch. Zero
	// value keeps tier 2 on; used to isolate the tiers when measuring.
	DisableFooterTier2 bool
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
	// bgzfRanges (if set) carries a large bgzf data file's index byte ranges for
	// tier-2 slice-precise seek extension (#107). The handle keeps its adaptive
	// readahead window regardless — the Format layer only adds ranges, it never
	// changes the detector (ruling 1); any overlap joins via the singleflight.
	bgzfRanges []bgzf.Range
	// footer-family tier-2 state (#108). footerKind is set at open when the key
	// names a footer-family container; the parse (footerMeta/footerZip) is lazy
	// on first read and shared across handles via footerState. footerParsed marks
	// that a parse was attempted (nil result = tier 1 only).
	footerKind footer.Format
	// footerStream: on a fat pipe the device-derived coalesce gap is large, so a
	// projection would coalesce to ≈ whole-file reads anyway — in-region, bytes
	// are free and round-trips aren't (session 30). Such a handle streams like a
	// plain file (readahead window on, whole-chunk reads, no extent/plan/batch),
	// matching base. When the gap is small (bandwidth-scarce), it stays byte-
	// precise. Decided once at open from the current gap vs the block size.
	footerStream  bool
	footerMu      sync.Mutex // guards the footer tier-2 state below (Read is concurrent per handle)
	footerParsed  bool
	footerMeta    *footer.ParquetMeta
	footerProj    *footerProjection
	footerZip     []footer.ZipEntry
	footerLastZip int

	// CargoShip virtual-file backing (#94). Non-nil when the index is
	// CargoShip-backed: the file's bytes live in one or more packed `.tar.zst`
	// chunks, and a read maps to frame range GETs on those chunks. A cargo handle
	// bypasses the bgzf/footer/sibling readahead paths entirely.
	cargo []cargoPart
	// lastReadEnd is the byte offset just past the previous read on this handle,
	// for the M16 step-1b detector-gap characterization/rule (byte gap = off -
	// lastReadEnd). Atomic because the kernel issues a handle's reads concurrently.
	lastReadEnd atomic.Int64
}

// cargoPart is one contiguous run of a virtual file's bytes in a packed chunk,
// with the chunk resolved to a Cargo-backed block-store Key.
type cargoPart struct {
	key         blockstore.Key
	archiveOff  int64 // start of this part's bytes in the chunk's uncompressed tar stream
	fileOff     int64 // start offset of this part within the virtual file
	length      int64
	uncompTotal int64 // chunk uncompressed total (objSize for the block store)
}

// liveIndex wraps the index.Reader so it can be swapped atomically (#167).
type liveIndex struct{ r index.Reader }

// IndexSwapper is implemented by the live filesystem: SwapIndex atomically
// replaces the index used for future lookups (refresh, #167). Open handles keep
// the index they resolved against, so in-flight reads are unaffected.
type IndexSwapper interface {
	SwapIndex(r index.Reader)
}

type rawFS struct {
	fuse.RawFileSystem // default (ENOSYS) for anything not overridden

	cfg Config
	// ix is the live index, held atomically so `lith refresh` (#167) can swap it
	// for a new published version under concurrent FUSE lookups. Reads go through
	// index(); the swap only affects SUBSEQUENT lookups/opens — an open handle
	// keeps the backing it captured (h.key, incl. the CargoShip frame table), so an
	// in-flight read completes against its own version's (immutable) objects.
	ix        atomic.Pointer[liveIndex]
	store     *blockstore.BlockStore
	met       *metrics.Metrics
	blockSize int64
	ctx       context.Context

	// server is captured in Init; SwapIndex (#167 refresh) uses it to invalidate
	// the kernel's entry/attr/page caches for entries that changed between
	// versions. nil before Init and in unit tests that never mount.
	server *fuse.Server

	// pfTrace, when non-nil (env LITH_PF_TRACE=<path>), receives one CSV line per
	// read of what the prefetch detector saw — the M16 step-1b characterization.
	// nil in production; no cost when unset.
	pfTrace   *os.File
	pfTraceMu sync.Mutex

	// missMu/missSeen dedup the ENOENT-lookup breadcrumb (#240): the first time a
	// lookup resolves to a path not in the index, log it at INFO so a prefix-
	// scoped mount that is silently short a key says which one. Deduped per
	// distinct path and capped so a probe-heavy app cannot flood the log.
	missMu   sync.Mutex
	missSeen map[string]struct{}

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

	// Zarr grid-aware readahead state (#70 tier 1).
	zarr *zarrState

	// bgzf-family readahead state (#107).
	bgzf *bgzfState

	// footer-family readahead state (#108).
	footer *footerState
}

// NewRawFileSystem builds the read-only RawFileSystem.
func NewRawFileSystem(cfg Config) fuse.RawFileSystem {
	f := &rawFS{
		RawFileSystem: fuse.NewDefaultRawFileSystem(),
		cfg:           cfg,
		store:         cfg.Store,
		met:           cfg.Metrics,
		blockSize:     cfg.Store.BlockSize(),
		ctx:           context.Background(),
		nodes:         map[uint64]*node{fuse.FUSE_ROOT_ID: {path: "", parent: fuse.FUSE_ROOT_ID, isDir: true}},
		handles:       map[uint64]*fileHandle{},
		nextFh:        1,
		sibLastPos:    map[string]int{},
		sibPendingSet: map[string]struct{}{},
		missSeen:      map[string]struct{}{},
		zarr:          newZarrState(),
		bgzf:          newBgzfState(),
		footer:        newFooterState(),
	}
	// Bound the pending-sibling tracker to a few readahead batches: a key that
	// falls out of it without being opened is counted as an unread sibling.
	f.sibPendingCap = cfg.SiblingReadahead * 4
	if f.sibPendingCap < 64 {
		f.sibPendingCap = 64
	}
	f.ix.Store(&liveIndex{r: cfg.Index})
	if p := os.Getenv("LITH_PF_TRACE"); p != "" {
		if tf, err := os.Create(p); err == nil {
			f.pfTrace = tf
			_, _ = fmt.Fprintln(tf, "key,off,len,blk,gap,state_before,state_after,peak_window")
		}
	}
	return f
}

// tracePF writes one characterization row (M16 step 1b). Serialized because the
// kernel issues a handle's reads concurrently.
func (f *rawFS) tracePF(key string, off, length, blk, gap int64, before, after prefetch.State, peak int64) {
	f.pfTraceMu.Lock()
	_, _ = fmt.Fprintf(f.pfTrace, "%s,%d,%d,%d,%d,%s,%s,%d\n", key, off, length, blk, gap, before, after, peak)
	f.pfTraceMu.Unlock()
}

// index returns the live index reader. Every lookup/open path reads through it so
// a concurrent SwapIndex is safe.
func (f *rawFS) index() index.Reader { return f.ix.Load().r }

// Init captures the fuse.Server so SwapIndex can drive kernel cache
// invalidation. go-fuse calls it once, during the mount handshake.
func (f *rawFS) Init(server *fuse.Server) { f.server = server }

// SwapIndex atomically replaces the index used for future lookups (#167 refresh)
// and then invalidates the kernel caches for entries that changed between the
// old and new version. Without that invalidation the mount's one-year entry and
// attribute timeouts and FOPEN_KEEP_CACHE would serve a reopen from the kernel's
// cached OLD-version attrs and pages — userspace would never see the read, and
// `lith refresh` would silently return stale data (#193). Open handles are
// unaffected — they captured their backing at open time.
//
// The diff is driven off the nodes the kernel actually knows about (those it has
// looked up), so an unchanged file is never invalidated — refresh keeps the
// cache it exists to preserve.
func (f *rawFS) SwapIndex(r index.Reader) {
	old := f.ix.Load().r
	f.ix.Store(&liveIndex{r: r})
	f.invalidateOnSwap(old, r)
}

// invalidateOnSwap tells the kernel to drop its cached view of every known entry
// whose identity changed between old and new. It must run OUTSIDE f.mu: a notify
// can re-enter the filesystem (Lookup), so it snapshots the node table under the
// lock and then notifies without it.
func (f *rawFS) invalidateOnSwap(old, updated index.Reader) {
	srv := f.server
	if srv == nil {
		return // not mounted (unit tests) — nothing cached in a kernel
	}
	type snap struct {
		ino, parent uint64
		path        string
		isDir       bool
	}
	f.mu.RLock()
	snaps := make([]snap, 0, len(f.nodes))
	for ino, n := range f.nodes {
		snaps = append(snaps, snap{ino: ino, parent: n.parent, path: n.path, isDir: n.isDir})
	}
	f.mu.RUnlock()

	for _, s := range snaps {
		if s.isDir {
			// A directory's cached attrs and readdir listing must be dropped only
			// if its child set changed (add/remove/type-flip). A child whose
			// content changed is handled by that child's own file node below, so
			// the dir listing itself is still valid — don't churn it.
			if dirIdentity(old, s.path) != dirIdentity(updated, s.path) {
				srv.InodeNotify(s.ino, 0, 0)
			}
			continue
		}
		if fileChanged(old, updated, s.path) {
			// InodeNotify(ino,0,0) invalidates attrs and (len<=0 → to EOF) the
			// whole page cache; EntryNotify drops the parent's name→ino dentry so
			// a reopen re-looks-up (covering a removed or type-changed entry).
			srv.InodeNotify(s.ino, 0, 0)
			srv.EntryNotify(s.parent, baseName(s.path))
		}
	}
}

// fileChanged reports whether a file at path differs between two index versions:
// gone, resized, or different content (ETag). Identical files are left cached.
func fileChanged(old, updated index.Reader, path string) bool {
	nf, nerr := updated.Stat(path)
	if nerr != nil {
		return true // removed in the new version
	}
	of, oerr := old.Stat(path)
	if oerr != nil {
		return true // appeared or type-changed
	}
	if of.Size != nf.Size {
		return true
	}
	return old.ETagHashOf(path) != updated.ETagHashOf(path)
}

// dirIdentity folds a directory's child set (name + inode) into one value so a
// swap can tell whether entries were added, removed, or type-flipped. Content
// changes to existing children don't move it — those are handled per-file.
func dirIdentity(r index.Reader, dir string) uint64 {
	h := uint64(1469598103934665603) // FNV-1a offset basis
	fold := func(b []byte) {
		for _, c := range b {
			h ^= uint64(c)
			h *= 1099511628211
		}
	}
	var cursor uint64
	for {
		ents, next, err := r.Readdir(dir, cursor, 4096)
		if err != nil {
			break
		}
		for _, e := range ents {
			fold([]byte(e.Name))
			var ino [8]byte
			for i := 0; i < 8; i++ {
				ino[i] = byte(e.Ino >> (8 * i))
			}
			fold(ino[:])
		}
		if next == 0 || len(ents) == 0 {
			break
		}
		cursor = next
	}
	return h
}

// baseName returns the last "/"-separated component of a lith relative path.
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (f *rawFS) observe(op string, start time.Time) {
	f.met.ObserveFUSE(op, time.Since(start).Seconds())
}

// recoverToStatus recovers a panic in a query-path handler and degrades it to
// EIO rather than letting it crash the single mount server for every user
// (defense-in-depth, H-a). It is deferred with the address of the handler's
// named return status; the success paths are untouched.
func (f *rawFS) recoverToStatus(status *fuse.Status) {
	if r := recover(); r != nil {
		slog.Error("lith: recovered panic in FUSE handler; returning EIO",
			"panic", r, "stack", string(debug.Stack()))
		*status = fuse.EIO
	}
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
func (f *rawFS) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) (status fuse.Status) {
	defer f.recoverToStatus(&status)
	defer f.observe("lookup", time.Now())
	parent, ok := f.resolve(header.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	childPath := joinPath(parent.path, name)
	fi, err := f.index().Stat("/" + childPath)
	if err != nil {
		f.logMiss(childPath)
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

// missLogCap bounds the number of distinct missing paths logged, so a probe-heavy
// app cannot flood the log or the dedup map (#240).
const missLogCap = 1024

// logMiss records, at INFO and once per distinct path, that a lookup resolved to
// a path not in the index — the breadcrumb a prefix-scoped read-only mount owes
// when it is silently short a key (#240). Deduped and capped; past the cap it is
// silent (the first misses are the diagnostic ones, and memory stays bounded).
func (f *rawFS) logMiss(path string) {
	f.missMu.Lock()
	if _, seen := f.missSeen[path]; seen || len(f.missSeen) >= missLogCap {
		f.missMu.Unlock()
		return
	}
	f.missSeen[path] = struct{}{}
	f.missMu.Unlock()
	slog.Info("lookup miss: path not under this mount's index", "path", "/"+path)
}

// GetAttr returns attributes for a NodeId.
func (f *rawFS) GetAttr(cancel <-chan struct{}, input *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	defer f.observe("getattr", time.Now())
	n, ok := f.resolve(input.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	fi, err := f.index().Stat("/" + n.path)
	if err != nil {
		return fuse.ENOENT
	}
	out.SetTimeout(oneYear)
	f.fillAttr(&out.Attr, fi)
	return fuse.OK
}

// Open opens a file for reading and allocates a handle with a prefetcher.
func (f *rawFS) Open(cancel <-chan struct{}, input *fuse.OpenIn, out *fuse.OpenOut) (status fuse.Status) {
	defer f.recoverToStatus(&status)
	n, ok := f.resolve(input.NodeId)
	if !ok {
		return fuse.ENOENT
	}
	fi, err := f.index().Stat("/" + n.path)
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
		key:  blockstore.Key{Key: f.objectKey(n.path), ETagHash: f.index().ETagHashOf("/" + n.path)},
		size: fi.Size,
		pf:   newPFWrapper(f.maxReadahead(), f.blockSize),
	}
	f.mu.Lock()
	fh := f.nextFh
	f.nextFh++
	f.handles[fh] = h
	f.mu.Unlock()

	// CargoShip-backed file (#94): its bytes live in packed `.tar.zst` chunks.
	// Resolve the read-mapping and stream the covering chunk region; a cargo
	// handle uses none of the object-key readahead paths below.
	if b, ok := f.index().BackingOf("/" + n.path); ok {
		for _, p := range b.Parts {
			h.cargo = append(h.cargo, cargoPart{
				key: blockstore.Key{Key: p.ChunkKey, ETagHash: p.ChunkETagHash,
					Cargo: &blockstore.CargoChunk{Frames: p.Frames, UncompTotal: p.ChunkUncompTotal}},
				archiveOff: p.ArchiveOffset, fileOff: p.FileOffset, length: p.Length, uncompTotal: p.ChunkUncompTotal,
			})
		}
		if len(h.cargo) > 0 {
			// Prefetch the first part's chunk region so a directory-order walk (files
			// packed in tree order) streams the chunk sequentially — the many-small-
			// objects win. Readahead operates on the chunk's uncompressed stream.
			p := h.cargo[0]
			base := p.archiveOff / f.blockSize
			for _, blk := range h.pf.open(f.perHandleWindow()) {
				b := base + blk
				go f.store.Prefetch(f.ctx, p.key, b, p.uncompTotal)
			}
			out.Fh = fh
			out.OpenFlags = fuse.FOPEN_KEEP_CACHE
			return fuse.OK
		}
	}

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
	// bgzf-family tier 1 (#107): an index-driven data file gets header prefetch +
	// random protection (no scan window); an index file gets a whole-index +
	// data-header prefetch. Detected from the Index (sibling presence), no S3 call.
	bgzfHandled := f.maybeBgzfReadahead(n.path, h, fi.Size)
	// footer-family tier 1 (#108): a columnar/archive container gets its footer
	// (tail) + head prefetched at open; tier-2 projection/entry prefetch is
	// driven from Read. Detected from the key extension, no S3 call.
	footerHandled := false
	if !bgzfHandled {
		footerHandled = f.maybeFooterReadahead(n.path, h, fi.Size)
	}
	// Big-pipe regime (session 30): if the device-derived coalesce gap is at least
	// a fill block, a projection coalesces to ≈ whole-file reads — cheaper to just
	// stream via the readahead window (like base) than to run the byte-precise
	// extent/plan path. Small gap (bandwidth-scarce) → stay byte-precise.
	if footerHandled && f.cfg.DisableFooterTier2 {
		// Tier 2 off (v0.3.0 default): byte-precise projection fetch is
		// experimental and slower than streaming on every tested box (#108), so a
		// footer handle streams like a plain one (full readahead, whole-chunk
		// reads). Otherwise it would suppress readahead and demand-fill 64 KiB
		// extents on a sequential scan (~100x slower). Only tier 1 stays.
		h.footerStream = true
	} else if footerHandled && f.store.CoalesceGap() >= f.store.BlockSize() {
		h.footerStream = true
		slog.Info("footer streaming regime (coalesce gap ≥ block, session-30 safety)",
			"path", n.path, "coalesce_gap", f.store.CoalesceGap(), "block_size", f.store.BlockSize())
	}

	// A byte-precise footer handle is projection-driven, not a sequential scan:
	// its tier-2 plan replaces the whole-block readahead window, which would
	// otherwise sweep every column. A streaming footer handle keeps the window
	// (it wants the whole file), like every non-footer handle.
	if fi.Size > f.partsThreshold() && (h.footerKind == footer.FormatNone || h.footerStream) {
		for _, pb := range h.pf.open(f.perHandleWindow()) {
			go f.store.Prefetch(f.ctx, h.key, pb, h.size)
		}
	}

	// If this directory is being walked in key order, read ahead across the
	// following siblings (#63). Skipped for bgzf (seek-driven) and Zarr (grid).
	if !bgzfHandled && !footerHandled && !f.maybeZarrReadahead(n.path) {
		f.maybeSiblingReadahead(n.path)
	}

	out.Fh = fh
	out.OpenFlags = fuse.FOPEN_KEEP_CACHE // content is immutable
	return fuse.OK
}

// Read serves a read from the block store and drives prefetch.
func (f *rawFS) Read(cancel <-chan struct{}, input *fuse.ReadIn, buf []byte) (rr fuse.ReadResult, status fuse.Status) {
	defer f.recoverToStatus(&status)
	start := time.Now()
	defer f.observe("read", start)
	f.mu.RLock()
	h := f.handles[input.Fh]
	f.mu.RUnlock()
	if h == nil {
		return nil, fuse.EBADF
	}

	off := int64(input.Offset)
	length := int64(len(buf))
	f.met.ObserveReadSize(length)
	end := off + length
	if end > h.size {
		end = h.size
	}

	// CargoShip-backed read (#94): map the file range to its part(s) in the packed
	// chunk(s) and serve from the frame-decode path; drive readahead on the chunk's
	// uncompressed stream so a directory-order walk streams sequentially.
	if h.cargo != nil {
		if off >= end {
			return fuse.ReadResultData(nil), fuse.OK
		}
		data, err := f.readCargo(h, off, end)
		if err != nil {
			return nil, fuse.EIO
		}
		return fuse.ReadResultData(data), fuse.OK
	}

	f.maybePartsFetch(h)
	// Record the distinct object bytes this read touches (#65).
	f.met.MarkDistinctRead(h.key.Key, off, length, h.size)

	// bgzf tier 2 (#107): extend a seek to the enclosing index range so the
	// tool's follow-on reads in that container/chunk are cache hits.
	if len(h.bgzfRanges) > 0 {
		f.bgzfSeekExtend(h, off)
	}
	// footer tier 2 (#108): prefetch this row group's projection columns
	// (Parquet) or this/next zip entries so follow-on reads are cache hits.
	if h.footerKind != footer.FormatNone && !f.cfg.DisableFooterTier2 && !h.footerStream {
		f.footerReadExtend(h, off, end)
		// Batch a burst of concurrent demand misses on this object into one
		// coalesced fetch (#124/session 30) — a front-loading reader (pyarrow
		// pre_buffer) issues many reads at once; without this each is its own tiny
		// GET. Skipped when the extents are already cached (no tick on a warm read).
		if end > off && !f.store.Covered(h.key, off, end-off, h.size) {
			// Cap the demand-batch coalesce gap: a byte-precise footer handle must not
			// let a burst of column reads merge across the non-projected columns between
			// them (#125 session 43 — the demand caller of the coalescer that #153's cap
			// missed).
			f.store.GatherDemand(f.ctx, h.key, off, end-off, h.size, blockstore.ProjectionCoalesceGap)
		}
	}

	var res fuse.ReadResult
	if off < end && off/blockstore.ChunkSize == (end-1)/blockstore.ChunkSize {
		// Read lies within one chunk: return a sub-slice of the (immutable)
		// chunk buffer directly — no copy, no allocation on a cache hit.
		ci := off / blockstore.ChunkSize
		lo := off - ci*blockstore.ChunkSize
		hi := lo + (end - off)
		// A byte-precise footer handle reads a projection: fetch only the read's
		// 64 KiB extents so a point read of a plan-prefetched column adds no S3
		// bytes (#118). Every other handle — plain, or a streaming footer handle on
		// a fat pipe — streams whole chunks.
		sequential := h.footerKind == footer.FormatNone || h.footerStream
		// #210/M16 step 1: a confirmed-random handle doing a small read fetches only
		// its 64 KiB extents, not the whole 1 MiB chunk — the scattered-metadata case
		// (HDF5/NetCDF-4 open reads hundreds of tiny fields spread across the object).
		// Streaming/sequential handles keep whole chunks and are byte-identical to
		// before, because a streaming handle never reaches Random; footer tier-2 is
		// already byte-exact (sequential==false) and untouched. The detector still
		// governs posture (it is not consulted to *change* the plan, only to pick the
		// fetch granularity for a demand read the plan does not cover).
		if sequential && h.pf.state() == prefetch.Random {
			if t := f.byteExactThreshold(); t > 0 && end-off <= t {
				sequential = false
			}
		}
		chunk, err := f.store.Chunk(f.ctx, h.key, ci, h.size, lo, hi, sequential)
		if err != nil {
			return nil, fuse.EIO
		}
		if hi > int64(len(chunk)) {
			hi = int64(len(chunk))
		}
		res = fuse.ReadResultData(chunk[lo:hi])
	} else {
		// Straddles a chunk boundary: assemble (a copy).
		f.met.ReadStraddle()
		data, err := f.store.GetRange(f.ctx, h.key, off, length, h.size)
		if err != nil {
			return nil, fuse.EIO
		}
		res = fuse.ReadResultData(data)
	}

	// Drive the prefetcher off the block this read falls in — unless a whole-file
	// parts fetch is in flight for this handle, which already covers every block
	// (see Open: readahead would only contend with the parts fetch), or this is a
	// footer-family handle whose byte-exact projection plan replaces the window
	// (a whole-block window would re-fetch the columns the plan skips; #118).
	if !h.partsDispatched.Load() && (h.footerKind == footer.FormatNone || h.footerStream) {
		blk := off / f.blockSize
		// Byte gap from the previous read on this handle: a large gap is a seek even
		// when it lands in an adjacent block or the grown reorder band, so the
		// detector does not grow the readahead window for a scattered metadata walk
		// (#210/M16 1b). The env-gated trace (LITH_PF_TRACE) records what the
		// detector saw for the 1b characterization; nil in production.
		gap := off - h.lastReadEnd.Load()
		var before prefetch.State
		if f.pfTrace != nil {
			before = h.pf.state()
		}
		pbs := h.pf.observe(blk, off, end-off, gap, f.perHandleWindow())
		if f.pfTrace != nil {
			f.tracePF(h.key.Key, off, end-off, blk, gap, before, h.pf.state(), h.pf.peakWindow())
		}
		for _, pb := range pbs {
			go f.store.Prefetch(f.ctx, h.key, pb, h.size)
		}
		h.lastReadEnd.Store(end)
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
	// #229: a whole-file parts fetch is a broad commit — do not make it at open,
	// before the access pattern is known. Wait until the coverage signal confirms
	// the handle tiles (Established): a file a reader streams whole establishes in
	// ~2 reads and is then parts-fetched as before; a sub-file reader (a hyperslab,
	// a footer probe) never establishes and is served precise, never whole-fetched.
	if !h.pf.isEstablished() {
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
	pos, ok := f.index().Position("/" + relPath)
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
		key := blockstore.Key{Key: f.index().Prefix() + s.Key, ETagHash: s.ETagHash}
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
		f.met.PrefetchSeeks(halvings, resets)
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

func (f *rawFS) readdir(input *fuse.ReadIn, out *fuse.DirEntryList, plus bool) (status fuse.Status) {
	defer f.recoverToStatus(&status)
	n, ok := f.resolve(input.NodeId)
	if !ok || !n.isDir {
		return fuse.ENOTDIR
	}
	self, err := f.index().Stat("/" + n.path)
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
				if pfi, e := f.index().Stat("/" + pn.path); e == nil {
					parentIno = pfi.Ino
				}
			}
			if !out.AddDirEntry(fuse.DirEntry{Mode: syscall.S_IFDIR, Name: "..", Ino: parentIno, Off: 2}) {
				return fuse.OK
			}
			cursor = 2
		}
	} else if cursor < 2 {
		// The plus path has no "."/".." entries, but it must still treat an
		// untrusted low offset the same as a fresh listing: a cursor of 1 (or 0)
		// would otherwise underflow rc = cursor - 2 to ~2^64 and hand Readdir a
		// bogus cursor. Clamp to the offset base (M1).
		cursor = 2 // keep the same offset base as the non-plus path
	}

	rc := cursor - 2
	for {
		ents, next, err := f.index().Readdir("/"+n.path, rc, 1)
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
			if fi, e2 := f.index().Stat("/" + childPath); e2 == nil {
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
	total := uint64(f.index().TotalSize())
	out.Bsize = bsize
	out.Frsize = bsize
	out.Blocks = (total + bsize - 1) / bsize
	out.Bfree = 0
	out.Bavail = 0
	out.Files = uint64(f.index().Len())
	out.Ffree = 0
	out.NameLen = 255
	return fuse.OK
}

func (f *rawFS) objectKey(relPath string) string {
	return f.index().Prefix() + relPath
}

func (f *rawFS) maxReadahead() int64 {
	if f.cfg.MaxReadahead > 0 {
		return f.cfg.MaxReadahead
	}
	return 32
}

// byteExactThreshold is the largest demand read that a confirmed-random handle
// fetches byte-exact (only its 64 KiB extents) instead of pulling the whole
// 1 MiB chunk (#210/M16 step 1). Derived from the device, not tuned: a
// mispredicted byte-exact fetch costs at most one extra round-trip if the handle
// later reads an adjacent extent, worth ~NIC×TTFB bytes of transfer, so that
// product is the size below which fetching only the read's extents is the safe
// bet. Clamped to [ExtentSize, ChunkSize]:
//   - fat pipe: NIC×TTFB exceeds a chunk, clamps to ChunkSize, so every
//     sub-chunk random read goes byte-exact — the transfer saving is ~free and
//     the Random posture already guards the clustering risk;
//   - thin pipe: the product shrinks, keeping larger reads whole-chunk so a
//     mispredicted extent re-fetch does not cross a slow link.
//
// Returns 0 (⇒ never byte-exact, whole-chunk preserved) when there is no device
// info — behavior-preserving for callers without a Limits policy.
func (f *rawFS) byteExactThreshold() int64 {
	if f.cfg.Limits == nil {
		return 0
	}
	dev := f.cfg.Limits.Device()
	if dev.NICBytesPerSec <= 0 || dev.TTFB <= 0 {
		return 0
	}
	rt := int64(float64(dev.NICBytesPerSec) * dev.TTFB.Seconds())
	if rt < blockstore.ExtentSize {
		rt = blockstore.ExtentSize
	}
	if rt > blockstore.ChunkSize {
		rt = blockstore.ChunkSize
	}
	return rt
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
