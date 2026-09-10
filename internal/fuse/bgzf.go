// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"sort"
	"strings"
	"sync"

	"github.com/scttfrdmn/lith/internal/blockstore"
	"github.com/scttfrdmn/lith/internal/format/bgzf"
)

// bgzf-family readahead (#107). See internal/format/bgzf for the mechanism and
// the parsers. Two tiers, both budget-bounded via Limits:
//
//   Tier 1 — on open of an index file, fetch it whole (small) plus the data
//   file's header block; on open of a data file that has an index sibling,
//   fetch the header block and put the handle in random-protection mode
//   (single-chunk fills), since index-driven access follows a seek, not a scan.
//
//   Tier 2 — parse the index (once, cached) into the set of compressed byte
//   ranges it points at; on a demand read at offset X, prefetch from X to the
//   end of the enclosing range (seek extension), so the tool's reads within the
//   container/chunk it just seeked to become cache hits.

// bgzfState caches, per data-file key, the sorted compressed ranges the index
// points at (nil once parsing has been attempted and failed → tier 1 only).
type bgzfState struct {
	mu      sync.Mutex
	ranges  map[string][]bgzf.Range // data rel key -> sorted, coalesced index ranges
	checked map[string]bool
}

func newBgzfState() *bgzfState {
	return &bgzfState{ranges: map[string][]bgzf.Range{}, checked: map[string]bool{}}
}

// reverse index-suffix → data-extension table for detecting an index-file open.
var bgzfIndexReverse = []struct {
	suffix   string
	kind     bgzf.IndexKind
	dataExts []string
}{
	{".crai", bgzf.KindCRAI, []string{".cram"}},
	{".bai", bgzf.KindBAI, []string{".bam"}},
	{".csi", bgzf.KindCSI, []string{".bam", ".bcf", ".vcf.gz"}},
	{".tbi", bgzf.KindTBI, []string{".vcf.gz"}},
}

// maybeBgzfReadahead applies tier-1 at open. Returns true when relPath is a
// bgzf data file with an index sibling or an index file itself (so the caller
// skips key-order sibling readahead, which is wrong for a seek-driven file).
func (f *rawFS) maybeBgzfReadahead(relPath string, h *fileHandle, size int64) bool {
	if f.cfg.Limits == nil {
		return false
	}
	// Case A: relPath is an index file for a data sibling.
	if dataRel, ok := f.bgzfDataFor(relPath); ok {
		f.met.FormatDetect("bgzf")
		f.prefetchByteRange(h.key, 0, size, size) // the whole (small) index
		f.met.FormatIndexPrefetchBytes(size)
		if dfi, err := f.ix.Stat("/" + dataRel); err == nil && !dfi.IsDir {
			dkey := blockstore.Key{Key: f.objectKey(dataRel), ETagHash: f.ix.ETagHashOf("/" + dataRel)}
			f.prefetchByteRange(dkey, 0, headerBytes(dfi.Size), dfi.Size) // data header
		}
		return true
	}
	// Case B: relPath is a data file with an index sibling present.
	cands := bgzf.IndexCandidates(relPath)
	if cands == nil {
		return false
	}
	var idxRel string
	var kind bgzf.IndexKind
	for _, c := range cands {
		if c.Kind == bgzf.KindNone {
			continue
		}
		if fi, err := f.ix.Stat("/" + c.Key); err == nil && !fi.IsDir {
			idxRel, kind = c.Key, c.Kind
			break
		}
	}
	if idxRel == "" {
		return false // no coordinate index sibling → leave to normal handling
	}
	f.met.FormatDetect("bgzf")
	h.randomProtect = true
	f.prefetchByteRange(h.key, 0, headerBytes(size), size) // header block
	h.bgzfRanges = f.bgzfRangesFor(idxRel, kind, size)     // tier-2 substrate (cached)
	return true
}

// bgzfDataFor returns the data-file key an index-file key indexes, if present.
func (f *rawFS) bgzfDataFor(relPath string) (string, bool) {
	lower := strings.ToLower(relPath)
	for _, e := range bgzfIndexReverse {
		if !strings.HasSuffix(lower, e.suffix) {
			continue
		}
		base := relPath[:len(relPath)-len(e.suffix)] // "x.bam.bai" -> "x.bam"
		if bgzf.IsDataFile(base) {
			if fi, err := f.ix.Stat("/" + base); err == nil && !fi.IsDir {
				return base, true
			}
		}
		for _, de := range e.dataExts { // "x.bai" -> "x.bam"
			if fi, err := f.ix.Stat("/" + base + de); err == nil && !fi.IsDir {
				return base + de, true
			}
		}
		return "", false
	}
	return "", false
}

// bgzfRangesFor parses the index sibling once (cached) and returns the sorted,
// coalesced compressed byte ranges it points at (for tier-2 seek extension).
func (f *rawFS) bgzfRangesFor(idxRel string, kind bgzf.IndexKind, dataSize int64) []bgzf.Range {
	f.bgzf.mu.Lock()
	if f.bgzf.checked[idxRel] {
		r := f.bgzf.ranges[idxRel]
		f.bgzf.mu.Unlock()
		return r
	}
	f.bgzf.mu.Unlock()

	var ranges []bgzf.Range
	fi, err := f.ix.Stat("/" + idxRel)
	if err == nil && !fi.IsDir && fi.Size > 0 && fi.Size <= bgzf.MaxIndexBytes {
		key := blockstore.Key{Key: f.objectKey(idxRel), ETagHash: f.ix.ETagHashOf("/" + idxRel)}
		if body, err := f.store.GetRange(f.ctx, key, 0, fi.Size, fi.Size); err == nil {
			switch kind {
			case bgzf.KindBAI:
				if ix, ok := bgzf.ParseBAI(body); ok {
					ranges = ix.AllRanges(dataSize)
				}
			case bgzf.KindTBI:
				if ix, ok := bgzf.ParseTBI(body); ok {
					ranges = ix.AllRanges(dataSize)
				}
			case bgzf.KindCSI:
				if ix, ok := bgzf.ParseCSI(body); ok {
					ranges = ix.AllRanges(dataSize)
				}
			case bgzf.KindCRAI:
				if ix, ok := bgzf.ParseCRAI(body); ok {
					ranges = ix.AllRanges(dataSize)
				}
			}
		}
	}
	f.bgzf.mu.Lock()
	f.bgzf.checked[idxRel] = true
	f.bgzf.ranges[idxRel] = ranges
	f.bgzf.mu.Unlock()
	return ranges
}

// bgzfSeekExtend is tier-2: on a demand read at compressed offset off, find the
// enclosing (or next) index range and prefetch from off to its end, so the
// tool's follow-on reads in the container/chunk it seeked to are cache hits.
func (f *rawFS) bgzfSeekExtend(h *fileHandle, off int64) {
	if len(h.bgzfRanges) == 0 {
		return
	}
	rs := h.bgzfRanges
	// First range whose End > off.
	i := sort.Search(len(rs), func(i int) bool { return rs[i].End > off })
	if i >= len(rs) {
		return
	}
	r := rs[i]
	start := off
	if r.Start > start {
		start = r.Start // off is between ranges: prefetch the next one
	}
	if r.End <= start {
		return
	}
	if f.prefetchByteRange(h.key, start, r.End, h.size) {
		f.met.FormatPlanRanges("bgzf")
	}
}

// prefetchByteRange prefetches the blocks covering [off,end) of key, reserving
// budget for the span. Returns false if the budget is exhausted.
func (f *rawFS) prefetchByteRange(key blockstore.Key, off, end, objSize int64) bool {
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
	first := off / f.blockSize
	last := (end - 1) / f.blockSize
	go func() {
		defer f.release(span)
		var wg sync.WaitGroup
		for b := first; b <= last; b++ {
			wg.Add(1)
			go func(b int64) {
				defer wg.Done()
				f.store.Prefetch(f.ctx, key, b, objSize)
			}(b)
		}
		wg.Wait()
	}()
	return true
}

// headerBytes is the leading span of a data file to prefetch (the header block
// every tool reads first), capped at the file size.
func headerBytes(size int64) int64 {
	const h = 1 << 20
	if size < h {
		return size
	}
	return h
}
