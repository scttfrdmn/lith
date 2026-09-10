// SPDX-License-Identifier: Apache-2.0

package bgzf

// CSI (used by BCF and large BAM/VCF) is a coordinate index like BAI/TBI but
// with a configurable binning scheme (min_shift, depth) and a per-bin "loffset"
// virtual offset that replaces BAI's separate linear index. The file is
// bgzf-compressed.

const (
	maxCSIDepth = 10
	csiMaxRefs  = 1 << 24
)

// CSIIndex is a parsed `.csi`: per-reference bins with chunks and loffsets, plus
// the binning parameters needed to compute overlapping bins for a region.
type CSIIndex struct {
	minShift int
	depth    int
	refs     []map[uint32]csiBin
}

type csiBin struct {
	loffset uint64
	chunks  []chunk
}

// NRef returns the number of references.
func (ix *CSIIndex) NRef() int { return len(ix.refs) }

// ParseCSI parses a `.csi` image (raw, bgzf-compressed on disk).
func ParseCSI(raw []byte) (*CSIIndex, bool) {
	b, ok := gunzip(raw)
	if !ok {
		return nil, false
	}
	r := newRdr(b)
	if !r.magic("CSI\x01") {
		return nil, false
	}
	minShift := int(r.i32())
	depth := int(r.i32())
	if !r.ok || minShift < 1 || minShift > 30 || depth < 0 || depth > maxCSIDepth {
		return nil, false
	}
	laux := r.i32()
	if !r.ok || laux < 0 {
		return nil, false
	}
	r.skip(int(laux))
	nref := r.i32()
	if !r.ok || nref < 0 || nref > csiMaxRefs {
		return nil, false
	}
	ix := &CSIIndex{minShift: minShift, depth: depth, refs: make([]map[uint32]csiBin, nref)}
	for i := int32(0); i < nref; i++ {
		nbin := r.i32()
		if !r.ok || nbin < 0 || nbin > maxBinsPerRef {
			return nil, false
		}
		m := make(map[uint32]csiBin, nbin)
		for j := int32(0); j < nbin; j++ {
			bin := r.u32()
			loffset := r.u64()
			nchunk := r.i32()
			if !r.ok || nchunk < 0 || nchunk > maxChunks {
				return nil, false
			}
			chunks := make([]chunk, 0, nchunk)
			for k := int32(0); k < nchunk; k++ {
				beg := r.u64()
				end := r.u64()
				chunks = append(chunks, chunk{beg, end})
			}
			if !r.ok {
				return nil, false
			}
			m[bin] = csiBin{loffset: loffset, chunks: chunks}
		}
		ix.refs[i] = m
	}
	if !r.ok {
		return nil, false
	}
	return ix, true
}

// csiReg2bins returns the bins overlapping [beg,end) for the CSI binning scheme.
func csiReg2bins(beg, end, minShift, depth int) []uint32 {
	if end <= beg {
		end = beg + 1
	}
	end--
	if beg < 0 {
		beg = 0
	}
	var out []uint32
	s := minShift + 3*depth
	t := 0
	for l := 0; l <= depth; l++ {
		lo := t + (beg >> uint(s))
		hi := t + (end >> uint(s))
		for k := lo; k <= hi; k++ {
			if k >= 0 {
				out = append(out, uint32(k))
			}
		}
		s -= 3
		t += 1 << uint(l*3)
	}
	return out
}

// RegionRanges resolves a region on reference refID to coalesced compressed byte
// ranges. The per-bin loffset is the floor (a chunk ending at/below it is
// irrelevant).
func (ix *CSIIndex) RegionRanges(refID, beg, end int, fileSize int64) []Range {
	if refID < 0 || refID >= len(ix.refs) || ix.refs[refID] == nil {
		return nil
	}
	ref := ix.refs[refID]
	var rs []Range
	for _, b := range csiReg2bins(beg, end, ix.minShift, ix.depth) {
		cb, ok := ref[b]
		if !ok {
			continue
		}
		for _, c := range cb.chunks {
			if c.end <= cb.loffset {
				continue
			}
			rs = append(rs, Range{Start: cvo(c.beg), End: cvo(c.end) + maxBlockSize})
		}
	}
	return coalesce(rs, maxBlockSize, fileSize)
}

// AllRanges returns every compressed byte range the index points at, coalesced.
func (ix *CSIIndex) AllRanges(fileSize int64) []Range {
	var rs []Range
	for _, ref := range ix.refs {
		for _, cb := range ref {
			for _, c := range cb.chunks {
				rs = append(rs, Range{Start: cvo(c.beg), End: cvo(c.end) + maxBlockSize})
			}
		}
	}
	return coalesce(rs, maxBlockSize, fileSize)
}
