// SPDX-License-Identifier: Apache-2.0

package blockstore

// Sparse chunk fills (#118). The cache unit stays ChunkSize (1 MiB), but an entry
// carries a filled-extent bitmap so a plan-driven range smaller than a chunk can
// fetch — and cache — only the bytes it covers, byte-exact. Extents are 64 KiB;
// a chunk has 16 of them, tracked in a uint16 (bit i = extent i is filled).
//
// Byte accounting for the criterion is about S3 bytes, not RAM: a partially
// filled chunk still holds a full ChunkSize buffer (unfilled extents are zero),
// so cache size accounting and the zero-copy read path are unchanged; only the
// bytes actually GET-ed shrink to the filled extents.

const (
	// ExtentSize is the fill granularity within a chunk.
	ExtentSize = 64 << 10
	// extentsPerChunk is ChunkSize / ExtentSize (16).
	extentsPerChunk = int(ChunkSize / ExtentSize)
	// fullExtents marks every extent of a whole chunk filled.
	fullExtents uint16 = 0xFFFF
)

// extentMask returns the bitmap of extents overlapping [lo, hi) within a chunk
// (byte offsets relative to the chunk start). lo/hi are clamped to [0, ChunkSize].
func extentMask(lo, hi int64) uint16 {
	if lo < 0 {
		lo = 0
	}
	if hi > ChunkSize {
		hi = ChunkSize
	}
	if hi <= lo {
		return 0
	}
	first := int(lo / ExtentSize)
	last := int((hi - 1) / ExtentSize)
	var m uint16
	for i := first; i <= last && i < extentsPerChunk; i++ {
		m |= 1 << uint(i)
	}
	return m
}

// maskForLen returns the extents covering [0, chunkLen) — a full 1 MiB chunk is
// fullExtents; a short trailing chunk covers only the extents its bytes reach.
func maskForLen(chunkLen int64) uint16 {
	if chunkLen >= ChunkSize {
		return fullExtents
	}
	return extentMask(0, chunkLen)
}

// covers reports whether every extent in want is set in have.
func covers(have, want uint16) bool { return have&want == want }

// extentByteRange returns the byte range [lo, hi) within a chunk of length cl
// spanned by the extents in want (first set bit to end of last set bit, clamped
// to cl). want must be non-zero.
func extentByteRange(want uint16, cl int64) (lo, hi int64) {
	first, last := -1, -1
	for i := 0; i < extentsPerChunk; i++ {
		if want&(1<<uint(i)) != 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, 0
	}
	lo = int64(first) * ExtentSize
	hi = int64(last+1) * ExtentSize
	if hi > cl {
		hi = cl
	}
	return lo, hi
}

// missingByteSpan returns the byte range [lo, hi) within a chunk that must be
// fetched to fill the extents in (want &^ have): from the first missing extent
// to the end of the last missing extent, clamped to chunkLen. Interior extents
// already filled inside that span are re-fetched (bounded, rare); returns
// ok=false when nothing is missing. The returned mask is the extents the span
// actually covers (what will be marked filled after the fetch).
func missingByteSpan(have, want uint16, chunkLen int64) (lo, hi int64, span uint16, ok bool) {
	miss := want &^ have
	if miss == 0 {
		return 0, 0, 0, false
	}
	first, last := -1, -1
	for i := 0; i < extentsPerChunk; i++ {
		if miss&(1<<uint(i)) != 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	lo = int64(first) * ExtentSize
	hi = int64(last+1) * ExtentSize
	if hi > chunkLen {
		hi = chunkLen
	}
	return lo, hi, extentMask(lo, hi), true
}
