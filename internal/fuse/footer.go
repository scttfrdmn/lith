// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"log/slog"
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
// and the row groups it has touched. Once the projection is confirmed the plan
// fires once, prefetching the projected column chunks of ALL remaining row groups
// in a single coalesced batch (#124/session 30). The already-touched row groups
// stay demand-served.
//
// colRGs records, per column, the distinct row groups a read's offset located it
// in. A genuinely-projected column is read in every row group, so it recurs; a
// column that only appears because pyarrow's pre_buffer coalesced a read across
// it lands in ~one row group. The plan therefore includes only columns seen in
// ≥2 row groups (#125): that excludes the coalesced-read noise that otherwise
// inflated the plan ~2.3× on scattered projections (session 42c: kind=plan was
// 265 MB for a 115 MB projection). Under-including a real column is safe — it is
// still served byte-exact on demand; over-including fetches bytes we never use.
//
// The plan fires only once the app has moved into a THIRD distinct row group. The
// recurrence filter can exclude noise only after two row groups are FULLY observed,
// and a row group is fully observed only when the app has moved past it — reading
// into the next one. Firing earlier (the instant the second row group is first
// touched) would see only that row group's first column recurred and wrongly drop
// the rest of the projection (whose second read has not happened yet).
type footerProjection struct {
	colRGs  map[string]map[int]bool // column -> distinct row groups it was located in
	rgsSeen map[int]bool
	maxRG   int  // highest row group the app has read; -1 = none yet
	planned bool // the one-shot full-projection plan has fired
}

func newFooterProjection() *footerProjection {
	return &footerProjection{colRGs: map[string]map[int]bool{}, rgsSeen: map[int]bool{}, maxRG: -1}
}

// projectedCols returns the columns confirmed by recurrence — located in at least
// minRGs distinct row groups — i.e. the true projection with coalesced-read noise
// removed.
func (p *footerProjection) projectedCols(minRGs int) []string {
	cols := make([]string, 0, len(p.colRGs))
	for c, rgs := range p.colRGs {
		if len(rgs) >= minRGs {
			cols = append(cols, c)
		}
	}
	return cols
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
	f.prefetchByteExact(h.key, tailStart, size, size)
	f.prefetchByteExact(h.key, 0, 8, size)
	h.footerKind = kind
	if kind == footer.FormatParquet {
		h.footerProj = newFooterProjection()
	}
	return true
}

// footerReadExtend is the tier-2 read hook. FUSE serves reads for one handle
// concurrently, and the parquet/zip paths mutate per-handle state (the parse
// result, the learned projection, the dispatched set), so the whole hook runs
// under the handle's footerMu. The one-time parse's GetRange runs under the lock
// too — it blocks only the first concurrent reads on this handle, once.
func (f *rawFS) footerReadExtend(h *fileHandle, off, end int64) {
	h.footerMu.Lock()
	defer h.footerMu.Unlock()
	switch h.footerKind {
	case footer.FormatParquet:
		f.footerParquetRead(h, off, end)
	case footer.FormatZip:
		f.footerZipRead(h, off)
	}
}

// footerParquetRead maps a demand read [off,end) to the column chunks it spans,
// learns the projection, and prefetches that row group's projection columns
// (current row group only — no cross-row-group speculation; see the package
// comment). It records EVERY column the read overlaps (LocateRange), not just the
// one at the start offset: a coalesced read spans several columns, and a read that
// begins in the padding before a column (off=0 precedes id.Start=4) still covers
// it — start-offset-only mapping missed the first column for any header-reading
// reader and mislearned a coalesced read as its leading column (#125 session 43).
func (f *rawFS) footerParquetRead(h *fileHandle, off, end int64) {
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
	p := h.footerProj
	if p.planned {
		return // one-shot plan already fired; remaining reads are demand-served
	}
	located := h.footerMeta.LocateRange(off, end)
	if len(located) == 0 {
		return // the footer itself or a gap between chunks
	}
	for _, l := range located {
		if p.colRGs[l.Path] == nil {
			p.colRGs[l.Path] = map[int]bool{}
		}
		p.colRGs[l.Path][l.RowGroup] = true
		p.rgsSeen[l.RowGroup] = true
		if l.RowGroup > p.maxRG {
			p.maxRG = l.RowGroup
		}
	}
	// Fire only once the app has read into a THIRD distinct row group, so two row
	// groups are fully observed and the recurrence filter is meaningful (#124/#125).
	// Then plan the projection for ALL remaining row groups (beyond the highest
	// touched) in ONE coalesced batch; the touched row groups stay demand-served.
	if len(p.rgsSeen) < 3 {
		return
	}
	// Include only columns confirmed by recurrence across ≥2 row groups — a
	// projected column recurs in every row group, while a column that only appears
	// because a coalesced pre_buffer read's offset landed in it does not. This is
	// the fix for the plan over-enumeration (#125): planning every located column
	// fetched ~2.3× the projection on scattered layouts.
	cols := p.projectedCols(2)
	if len(cols) == 0 {
		return // no column confirmed across ≥2 row groups yet; keep observing (do not fire)
	}
	p.planned = true
	nRG := len(h.footerMeta.RowGroups)
	ranges := h.footerMeta.ProjectionChunks(cols, p.maxRG+1, nRG)
	if slog.Default().Enabled(f.ctx, slog.LevelDebug) {
		f.logProjectionPlan(h, p, ranges, nRG)
	}
	f.footerPrefetch(h, ranges, "parquet")
}

// logProjectionPlan emits per-column diagnostics at the plan fire (#125): for
// EVERY located column its name, the distinct row groups it recurred in, whether
// the ≥2-RG filter planned it, and its planned bytes over [maxRG+1, nRG). This
// resolves, from one bench run, which regime the projection is in:
//
//   - noise columns present with recurrence counts ≈ the projection's → pre_buffer
//     sweeps the same adjacent span every row group; the filter cannot help and the
//     fix is replaying observed byte ranges, not reasoning about columns;
//   - only the projected columns planned and plan bytes near the true projection →
//     the filter worked;
//   - only projected columns planned but bytes still high → something else inflates,
//     and the per-column bytes name it.
func (f *rawFS) logProjectionPlan(h *fileHandle, p *footerProjection, ranges []footer.Range, nRG int) {
	var planBytes int64
	for _, r := range ranges {
		planBytes += r.End - r.Start
	}
	names := make([]string, 0, len(p.colRGs))
	for c := range p.colRGs {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		var colBytes int64
		for _, r := range h.footerMeta.ProjectionChunks([]string{c}, p.maxRG+1, nRG) {
			colBytes += r.End - r.Start
		}
		slog.Debug("footer projection column", "col", c, "rgcount", len(p.colRGs[c]),
			"planned", len(p.colRGs[c]) >= 2, "col_bytes", colBytes)
	}
	slog.Debug("footer projection plan", "planned_cols", len(p.projectedCols(2)),
		"located_cols", len(p.colRGs), "plan_bytes", planBytes, "plan_MB", planBytes/(1<<20),
		"from_rg", p.maxRG+1, "to_rg", nRG)
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

// footerPrefetch dispatches projection/entry ranges as one budget-bounded,
// gap-tolerant coalesced batch (#124): the blockstore merges them across chunk
// boundaries into a few range GETs (device-derived gap) rather than one tiny GET
// per column chunk.
func (f *rawFS) footerPrefetch(h *fileHandle, ranges []footer.Range, format string) {
	if len(ranges) == 0 {
		return
	}
	batch := make([]blockstore.Range, 0, len(ranges))
	var total int64
	for _, r := range ranges {
		batch = append(batch, blockstore.Range{Start: r.Start, End: r.End})
		total += r.End - r.Start
		f.met.FormatPlanRanges(format)
	}
	if len(batch) == 0 || !f.reserve(total) {
		return
	}
	go func() {
		defer f.release(total)
		f.store.FillBatch(f.ctx, h.key, batch, h.size, blockstore.ProjectionCoalesceGap)
	}()
}

// prefetchByteExact reserves prefetch budget for [off,end) and fills exactly the
// 64 KiB extents it covers, byte-exact, via the blockstore (#118) — not the
// whole blocks prefetchByteRange would pull. Returns false if the budget is
// exhausted.
func (f *rawFS) prefetchByteExact(key blockstore.Key, off, end, objSize int64) bool {
	if off < 0 {
		off = 0
	}
	if end > objSize {
		end = objSize
	}
	if end <= off {
		return true
	}
	span := end - off
	if !f.reserve(span) {
		return false
	}
	go func() {
		defer f.release(span)
		f.store.FillRange(f.ctx, key, off, end, objSize)
	}()
	return true
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
