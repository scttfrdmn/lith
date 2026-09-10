// SPDX-License-Identifier: Apache-2.0

package bgzf

import (
	"strconv"
	"strings"
)

// CRAI (CRAM index) is a gzip-compressed TSV, one line per slice:
//   seqId  alnStart  alnSpan  containerOffset  sliceOffset  sliceSize
// (all integers). To serve a region we prefetch the byte range of every slice
// whose alignment span overlaps the region, plus the container header.

const maxCRAILines = 1 << 24

// CRAIEntry is one parsed CRAI slice record.
type CRAIEntry struct {
	SeqID           int
	Start           int64
	Span            int64
	ContainerOffset int64
	SliceOffset     int64
	SliceSize       int64
}

// CRAIIndex is the parsed set of CRAI entries.
type CRAIIndex struct {
	entries []CRAIEntry
}

// Len returns the number of slice records.
func (ix *CRAIIndex) Len() int { return len(ix.entries) }

// ParseCRAI parses a `.crai` image (raw, gzip-compressed on disk). Malformed
// lines are skipped; (nil,false) only if nothing parses or the input is
// oversize/undecompressable.
func ParseCRAI(raw []byte) (*CRAIIndex, bool) {
	b, ok := gunzip(raw)
	if !ok {
		return nil, false
	}
	ix := &CRAIIndex{}
	lines := strings.Split(string(b), "\n")
	if len(lines) > maxCRAILines {
		return nil, false
	}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) < 6 {
			continue
		}
		vals := make([]int64, 6)
		bad := false
		for i := 0; i < 6; i++ {
			v, err := strconv.ParseInt(f[i], 10, 64)
			if err != nil {
				bad = true
				break
			}
			vals[i] = v
		}
		if bad {
			continue
		}
		if vals[3] < 0 || vals[4] < 0 || vals[5] < 0 {
			continue // negative byte offsets/sizes are impossible
		}
		ix.entries = append(ix.entries, CRAIEntry{
			SeqID: int(vals[0]), Start: vals[1], Span: vals[2],
			ContainerOffset: vals[3], SliceOffset: vals[4], SliceSize: vals[5],
		})
	}
	if len(ix.entries) == 0 {
		return nil, false
	}
	return ix, true
}

// RegionRanges returns the coalesced byte ranges of the slices on reference
// refID whose alignment span overlaps [beg,end) (0-based half-open). Each slice
// range covers its container from the container offset through the end of the
// slice, so the container header is included.
func (ix *CRAIIndex) RegionRanges(refID, beg, end int, fileSize int64) []Range {
	var rs []Range
	for _, e := range ix.entries {
		if e.SeqID != refID {
			continue
		}
		// Overlap [Start, Start+Span) with [beg,end). CRAM/CRAI coordinates may be
		// 1-based; a one-position slop is harmless (we over-prefetch, guardrail
		// catches waste) and avoids missing an edge slice.
		s := e.Start
		en := e.Start + e.Span
		if en <= int64(beg) || s > int64(end) {
			continue
		}
		// Slice-precise (ruling 3): the slice occupies
		// [containerOffset+sliceOffset, +sliceSize) — prefetch exactly that, not
		// the whole container (which was the 4× over-fetch). Adjacent slices
		// coalesce.
		start := e.ContainerOffset + e.SliceOffset
		rs = append(rs, Range{Start: start, End: start + e.SliceSize})
	}
	return coalesce(rs, maxBlockSize, fileSize)
}

// AllRanges returns every slice byte range in the index as sorted per-slice
// units (not coalesced). CRAM slices are physically contiguous, so coalescing
// would fuse them into one whole-file range; keeping per-slice boundaries lets
// tier-2 seek-extension prefetch just the enclosing slice (ruling 3, near-zero
// over-fetch). See units.
func (ix *CRAIIndex) AllRanges(fileSize int64) []Range {
	var rs []Range
	for _, e := range ix.entries {
		start := e.ContainerOffset + e.SliceOffset // slice-precise (ruling 3)
		rs = append(rs, Range{Start: start, End: start + e.SliceSize})
	}
	return units(rs, fileSize)
}
