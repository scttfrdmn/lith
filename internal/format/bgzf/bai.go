// SPDX-License-Identifier: Apache-2.0

package bgzf

// BAI and TBI share the same per-reference index body: a set of bins (each a
// list of [beg,end) virtual-offset chunks) plus a linear index (one virtual
// offset per 16 kb window). They differ only in the file header. This file
// parses both into a common form and resolves a region to compressed byte
// ranges, using the standard UCSC binning scheme (min_shift 14, 6 levels).

const (
	linearShift   = 14 // 16 kb linear-index windows
	maxRefs       = 1 << 24
	maxBinsPerRef = 1 << 20
	maxChunks     = 1 << 24
	maxIntervals  = 1 << 24
)

// chunk is a [beg,end) pair of bgzf virtual offsets.
type chunk struct{ beg, end uint64 }

// refIndex is one reference's bins and linear index.
type refIndex struct {
	bins   map[uint32][]chunk
	linear []uint64 // linear[i] = smallest virtual offset overlapping window i
}

// Index is a parsed BAI/TBI: per-reference bins + linear index.
type Index struct {
	Kind IndexKind
	refs []*refIndex
}

// NRef returns the number of references in the index.
func (ix *Index) NRef() int { return len(ix.refs) }

// ParseBAI parses a BAM `.bai` image. Bounds-checked; (nil,false) on any problem.
func ParseBAI(b []byte) (*Index, bool) {
	if len(b) == 0 || len(b) > MaxIndexBytes {
		return nil, false
	}
	r := newRdr(b)
	if !r.magic("BAI\x01") {
		return nil, false
	}
	ix, ok := parseRefs(r, KindBAI)
	if !ok {
		return nil, false
	}
	return ix, true
}

// ParseTBI parses a tabix `.tbi` image (raw, bgzf-compressed on disk).
func ParseTBI(raw []byte) (*Index, bool) {
	b, ok := gunzip(raw)
	if !ok {
		return nil, false
	}
	r := newRdr(b)
	if !r.magic("TBI\x01") {
		return nil, false
	}
	// tabix header: format, col_seq, col_beg, col_end, meta, skip (6×int32),
	// then l_nm + names.
	r.skip(6 * 4)
	lnm := r.i32()
	if lnm < 0 {
		return nil, false
	}
	r.skip(int(lnm))
	ix, ok := parseRefs(r, KindTBI)
	if !ok {
		return nil, false
	}
	return ix, true
}

// parseRefs reads the shared per-reference bin/chunk/linear body.
func parseRefs(r *rdr, kind IndexKind) (*Index, bool) {
	nref := r.i32()
	if !r.ok || nref < 0 || nref > maxRefs {
		return nil, false
	}
	ix := &Index{Kind: kind, refs: make([]*refIndex, nref)}
	for i := int32(0); i < nref; i++ {
		ref := &refIndex{bins: map[uint32][]chunk{}}
		nbin := r.i32()
		if !r.ok || nbin < 0 || nbin > maxBinsPerRef {
			return nil, false
		}
		for j := int32(0); j < nbin; j++ {
			bin := r.u32()
			nchunk := r.i32()
			if !r.ok || nchunk < 0 || nchunk > maxChunks {
				return nil, false
			}
			chunks := make([]chunk, 0, nchunk)
			for k := int32(0); k < nchunk; k++ {
				beg := r.u64()
				end := r.u64()
				if !r.ok {
					return nil, false
				}
				chunks = append(chunks, chunk{beg, end})
			}
			ref.bins[bin] = chunks
		}
		nintv := r.i32()
		if !r.ok || nintv < 0 || nintv > maxIntervals {
			return nil, false
		}
		ref.linear = make([]uint64, nintv)
		for k := int32(0); k < nintv; k++ {
			ref.linear[k] = r.u64()
		}
		if !r.ok {
			return nil, false
		}
		ix.refs[i] = ref
	}
	return ix, r.ok
}

// reg2bins returns the bins that may overlap [beg,end) under the standard
// 6-level UCSC scheme (min_shift 14). beg/end are 0-based.
func reg2bins(beg, end int) []uint32 {
	if end <= beg {
		end = beg + 1
	}
	end--
	if beg < 0 {
		beg = 0
	}
	out := []uint32{0}
	add := func(base, sh int) {
		lo := base + (beg >> sh)
		hi := base + (end >> sh)
		for k := lo; k <= hi; k++ {
			if k >= 0 {
				out = append(out, uint32(k))
			}
		}
	}
	add(1, 26)
	add(9, 23)
	add(73, 20)
	add(585, 17)
	add(4681, 14)
	return out
}

// RegionRanges resolves a region on reference refID to the coalesced compressed
// byte ranges to prefetch. beg/end are 0-based half-open coordinates. Chunks are
// filtered by the linear-index floor (a chunk ending before the first record of
// the region's first 16 kb window cannot contain it) and coalesced within one
// bgzf block of slack, clamped to fileSize.
func (ix *Index) RegionRanges(refID, beg, end int, fileSize int64) []Range {
	if refID < 0 || refID >= len(ix.refs) || ix.refs[refID] == nil {
		return nil
	}
	ref := ix.refs[refID]

	// Linear-index floor: the smallest virtual offset overlapping the region's
	// first window; chunks ending at/below it are irrelevant.
	var minOff uint64
	if len(ref.linear) > 0 {
		w := beg >> linearShift
		if w < 0 {
			w = 0
		}
		if w >= len(ref.linear) {
			w = len(ref.linear) - 1
		}
		minOff = ref.linear[w]
	}

	var rs []Range
	for _, b := range reg2bins(beg, end) {
		for _, c := range ref.bins[b] {
			if c.end <= minOff {
				continue
			}
			// The compressed range spans from the chunk's begin block to the
			// chunk's end block plus one block (the end offset may point mid-block).
			rs = append(rs, Range{Start: cvo(c.beg), End: cvo(c.end) + maxBlockSize})
		}
	}
	return coalesce(rs, maxBlockSize, fileSize)
}

// AllRanges returns every compressed byte range the index points at, coalesced —
// the union over all references. Tier-2 seek-extension uses it to map a demand
// read offset to the enclosing chunk range.
func (ix *Index) AllRanges(fileSize int64) []Range {
	var rs []Range
	for _, ref := range ix.refs {
		if ref == nil {
			continue
		}
		for _, chunks := range ref.bins {
			for _, c := range chunks {
				rs = append(rs, Range{Start: cvo(c.beg), End: cvo(c.end) + maxBlockSize})
			}
		}
	}
	return coalesce(rs, maxBlockSize, fileSize)
}
