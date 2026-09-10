// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sort"
	"sync"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/format/footer"
)

// Footer-family readahead (#108). See internal/format/footer for the parsers.
//
//	Tier 1 (all formats, generic): on open, prefetch the last 64 KiB (the footer
//	every reader reads first) and the first 8 bytes (the head magic). Detected by
//	extension; the tier-2 parse confirms the magic before trusting the bytes.
//
//	Tier 2, Parquet: parse the footer (row groups × column chunks). Learn the
//	app's projection from the columns it demands and, when the app reads a row
//	group, prefetch that row group's projection columns in parallel (coalesced)
//	so the tool's follow-on per-column reads are cache hits. We deliberately do
//	NOT speculate across row groups: a row group the app never touches (predicate
//	pushdown skipped it on the footer statistics) is never fetched, so lith's byte
//	footprint never exceeds a projection-and-predicate-filtered reader's.
//
//	Tier 2, zip: parse the central directory; on read of an entry, prefetch its
//	local header + compressed data, and when entries are read in directory order,
//	read ahead across the next few entries (sibling readahead within one object).
//
// ORC and Arrow are tier-1 only this session (tier-2 tracked as follow-ups).

// footerZipReadahead is how many following zip entries a directory-order read
// prefetches.
const footerZipReadahead = 8

// footerState caches parsed footer metadata per data-file key so concurrent
// handles on one object share a single parse (nil once a parse has been
// attempted and failed → tier 1 only).
type footerState struct {
	mu      sync.Mutex
	parquet map[string]*footer.ParquetMeta
	zip     map[string][]footer.ZipEntry
	checked map[string]bool
}

func newFooterState() *footerState {
	return &footerState{
		parquet: map[string]*footer.ParquetMeta{},
		zip:     map[string][]footer.ZipEntry{},
		checked: map[string]bool{},
	}
}

// footerProjection tracks, per handle, the Parquet columns the app has demanded
// and the plan ranges already dispatched (so re-reads don't re-prefetch).
type footerProjection struct {
	cols       map[string]bool
	dispatched map[int64]bool // range Start -> dispatched
}

func newFooterProjection() *footerProjection {
	return &footerProjection{cols: map[string]bool{}, dispatched: map[int64]bool{}}
}

// maybeFooterReadahead applies tier 1 at open and arms tier 2. Returns true when
// relPath is a footer-family data file (so the caller skips key-order sibling
// readahead, which is wrong for a seek/projection-driven file).
func (f *rawFS) maybeFooterReadahead(relPath string, h *fileHandle, size int64) bool {
	if f.cfg.Limits == nil || size <= 0 {
		return false
	}
	kind := footer.Detect(relPath)
	if kind == footer.FormatNone {
		return false
	}
	f.met.FormatDetect(kind.String())
	// Tier 1: the footer (tail) and the head magic — what every reader reads
	// first. Confirmation of the magic happens in the tier-2 parse.
	tailStart := size - footer.TailBytes
	if tailStart < 0 {
		tailStart = 0
	}
	f.prefetchByteRange(h.key, tailStart, size, size)
	f.prefetchByteRange(h.key, 0, 8, size)
	h.footerKind = kind
	if kind == footer.FormatParquet {
		h.footerProj = newFooterProjection()
	}
	return true
}

// footerReadExtend is the tier-2 read hook.
func (f *rawFS) footerReadExtend(h *fileHandle, off int64) {
	switch h.footerKind {
	case footer.FormatParquet:
		f.footerParquetRead(h, off)
	case footer.FormatZip:
		f.footerZipRead(h, off)
	}
}

// footerParquetRead maps a demand read to (row group, column), learns the
// projection, and prefetches that row group's projection columns (current row
// group only — no cross-row-group speculation; see the package comment).
func (f *rawFS) footerParquetRead(h *fileHandle, off int64) {
	if h.footerProj == nil {
		return
	}
	if h.footerMeta == nil {
		if h.footerParsed {
			return // parse already attempted and failed → tier 1 only
		}
		h.footerParsed = true
		h.footerMeta = f.footerParquetMeta(h.key, h.size)
		if h.footerMeta == nil {
			return
		}
	}
	rg, col, ok := h.footerMeta.Locate(off)
	if !ok {
		return // the footer itself or a gap between chunks
	}
	if !h.footerProj.cols[col] {
		h.footerProj.cols[col] = true
	}
	cols := make([]string, 0, len(h.footerProj.cols))
	for c := range h.footerProj.cols {
		cols = append(cols, c)
	}
	f.footerPrefetch(h, h.footerMeta.ProjectionChunks(cols, rg, rg+1), "parquet")
}

// footerZipRead prefetches the entry a read falls in (local header + data) and,
// on a directory-order read, the next few entries.
func (f *rawFS) footerZipRead(h *fileHandle, off int64) {
	if h.footerZip == nil {
		if h.footerParsed {
			return
		}
		h.footerParsed = true
		h.footerZip = f.footerZipEntries(h.key, h.size)
		if h.footerZip == nil {
			return
		}
	}
	entries := h.footerZip
	// entries are in central-directory order, which is by local header offset for
	// normal archives; find the entry whose local header starts at or before off.
	i := sort.Search(len(entries), func(i int) bool { return entries[i].LocalHeaderOffset > off }) - 1
	if i < 0 {
		return
	}
	f.footerPrefetch(h, []footer.Range{zipEntryRange(entries[i])}, "zip")
	if i >= h.footerLastZip { // reading forward through the directory
		for j := i + 1; j <= i+footerZipReadahead && j < len(entries); j++ {
			f.footerPrefetch(h, []footer.Range{zipEntryRange(entries[j])}, "zip")
		}
	}
	h.footerLastZip = i
}

// zipEntryRange is the byte range of a zip entry: local header + name + a slack
// for the (unknown-here) extra field + the compressed data.
func zipEntryRange(e footer.ZipEntry) footer.Range {
	const localHeaderFixed = 30 // signature..name-len fixed header
	const extraSlack = 256      // local extra field may differ from the central one
	start := e.LocalHeaderOffset
	end := start + localHeaderFixed + int64(len(e.Name)) + extraSlack + e.CompressedSize
	return footer.Range{Start: start, End: end}
}

// footerParquetMeta parses (and caches) the Parquet footer for key. Returns nil
// if the magic does not confirm or the footer does not parse (→ tier 1 only).
func (f *rawFS) footerParquetMeta(key blockstore.Key, size int64) *footer.ParquetMeta {
	f.footer.mu.Lock()
	if f.footer.checked[key.Key] {
		m := f.footer.parquet[key.Key]
		f.footer.mu.Unlock()
		return m
	}
	f.footer.mu.Unlock()

	m := f.parseParquet(key, size)

	f.footer.mu.Lock()
	f.footer.checked[key.Key] = true
	f.footer.parquet[key.Key] = m
	f.footer.mu.Unlock()
	return m
}

func (f *rawFS) parseParquet(key blockstore.Key, size int64) *footer.ParquetMeta {
	tailStart := size - footer.TailBytes
	if tailStart < 0 {
		tailStart = 0
	}
	tail, err := f.store.GetRange(f.ctx, key, tailStart, size-tailStart, size)
	if err != nil {
		return nil
	}
	head, err := f.store.GetRange(f.ctx, key, 0, min64(8, size), size)
	if err != nil || !footer.ConfirmMagic(footer.FormatParquet, head, tail, size) {
		return nil
	}
	footerStart, wholeInTail, ok := footer.ParquetFooterStart(tail, size)
	if !ok {
		return nil
	}
	var meta []byte
	if wholeInTail {
		lo := footerStart - tailStart
		hi := (size - 8) - tailStart
		if lo < 0 || hi > int64(len(tail)) || lo > hi {
			return nil
		}
		meta = tail[lo:hi]
	} else {
		if meta, err = f.store.GetRange(f.ctx, key, footerStart, (size-8)-footerStart, size); err != nil {
			return nil
		}
	}
	m, ok := footer.ParseParquetFooter(meta)
	if !ok || len(m.RowGroups) == 0 {
		return nil
	}
	return m
}

func (f *rawFS) footerZipEntries(key blockstore.Key, size int64) []footer.ZipEntry {
	f.footer.mu.Lock()
	if f.footer.checked[key.Key] {
		e := f.footer.zip[key.Key]
		f.footer.mu.Unlock()
		return e
	}
	f.footer.mu.Unlock()

	var entries []footer.ZipEntry
	tailStart := size - footer.TailBytes
	if tailStart < 0 {
		tailStart = 0
	}
	if tail, err := f.store.GetRange(f.ctx, key, tailStart, size-tailStart, size); err == nil {
		if e, ok := footer.ParseZipCentralDir(tail, size); ok {
			entries = e
		}
	}
	f.footer.mu.Lock()
	f.footer.checked[key.Key] = true
	f.footer.zip[key.Key] = entries
	f.footer.mu.Unlock()
	return entries
}

// footerPrefetch coalesces adjacent ranges and dispatches each as a budget-
// bounded prefetch, deduped per handle so re-reads don't re-issue. Each
// dispatched range counts against the plan-ranges metric (#108).
func (f *rawFS) footerPrefetch(h *fileHandle, ranges []footer.Range, format string) {
	if len(ranges) == 0 {
		return
	}
	coalesced := coalesceFooter(ranges, f.blockSize)
	for _, r := range coalesced {
		if h.footerProj != nil {
			if h.footerProj.dispatched[r.Start] {
				continue
			}
			h.footerProj.dispatched[r.Start] = true
		}
		if f.prefetchByteRange(h.key, r.Start, r.End, h.size) {
			f.met.FormatPlanRanges(format)
		}
	}
}

// coalesceFooter sorts ranges by Start and merges those within slack bytes of
// each other, so a row group's adjacent column chunks become one range GET.
func coalesceFooter(rs []footer.Range, slack int64) []footer.Range {
	if len(rs) == 0 {
		return nil
	}
	cp := append([]footer.Range(nil), rs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Start < cp[j].Start })
	out := []footer.Range{cp[0]}
	for _, r := range cp[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+slack {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
