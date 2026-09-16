// SPDX-License-Identifier: Apache-2.0

// Package prefetch implements lith's per-open-file-handle access-pattern
// detector and readahead planner. The planner keeps a dispatch frontier that
// leads the demand cursor: blocks are dispatched on open and on every window
// advance, never reactively on the miss that reveals a gap. See the pinned
// Design issue, §4.3, and issues #38 and #49.
//
// The detector is tolerant of out-of-order reads: the kernel issues a single
// file handle's readahead concurrently, so reads can arrive at the FUSE layer
// out of order even for a strictly sequential file. A read within the reorder
// band [cursor-window, frontier+window] is treated as sequential progress; only
// a genuine out-of-band jump is a seek, which halves the window (floor 2)
// rather than collapsing it — the full re-ramp was the #49 lone-survivor tail.
package prefetch

import (
	"math"
	"sort"
)

// State is the detected access pattern for a file handle.
type State int

const (
	// Cold is the initial state, before a pattern is established.
	Cold State = iota
	// Sequential means consecutive (or in-band reordered) blocks; readahead
	// grows geometrically.
	Sequential
	// Strided means a constant non-unit block delta seen at least twice.
	Strided
	// Random means no detectable pattern; nothing is prefetched.
	Random
)

func (s State) String() string {
	switch s {
	case Cold:
		return "cold"
	case Sequential:
		return "sequential"
	case Strided:
		return "strided"
	default:
		return "random"
	}
}

// initialWindow is the number of blocks dispatched at open and after a seek.
const initialWindow = 2

// Prefetcher tracks one file handle's read pattern and returns the block
// indices that should be prefetched. It maintains a dispatch frontier (the next
// block index not yet dispatched) so the readahead window stays ahead of the
// demand cursor.
//
//	cold -> sequential (unit step or in-band reorder)
//	     -> strided    (constant non-unit delta observed twice)
//	     -> random     (two out-of-band seeks with no progress between)
//
// Sequential grows its window geometrically from 2 up to maxReadahead; a single
// out-of-band seek halves the window and re-anchors; strided dispatches the
// single next predicted block.
type Prefetcher struct {
	maxReadahead int64

	haveLast  bool
	lastBlock int64
	lastDelta int64
	state     State
	window    int64
	cursor    int64 // highest block demanded
	frontier  int64 // next block index not yet dispatched
	pending   bool  // an unexplained jump is outstanding (one more seeks -> random)
	seqGapMax int64 // largest byte gap that still counts as sequential (#210/M16 1b); MaxInt64 = off

	// Coverage gate (#221): a stream's reads *tile* — over a trailing window they
	// cover most of the span between them — while a scattered walk (a metadata
	// traversal, a GRIB .idx field sweep, a COG overview) leaves most of the span
	// untouched, however sequential its block deltas look. When coverage over the
	// last covWindow reads falls below covMin, the handle is punctate and is forced
	// Random so the seek path stops re-anchoring Sequential and prefetching two
	// blocks at every landing (which refilled whole sub-64-MiB objects, #220).
	// covMin<=0 disables the gate (behaviour-preserving default).
	covWindow int
	covMin    float64
	covSpans  []covSpan // ring of the last covWindow reads (byte ranges)

	// established (#229) is the single signal all three broad-fetch entry points
	// gate on: the open-time ramp, the readahead-window growth, and open-time
	// parts-fetch (the last in the FUSE layer, via Established()). It is true once
	// the coverage signal has confirmed the access tiles — never before, so a
	// handle whose pattern is not yet known is served precise (byte-exact /
	// single-chunk) and commits no broad fetch. It reverts to false when coverage
	// collapses (a stream that becomes a walk), inheriting the #221 transition.
	established bool

	// Diagnostics (#49). Single-threaded via pfWrapper.
	halvings    int64 // window halvings on a seek from an established pattern
	resetRandom int64 // collapses to Random (a second seek with no progress)
	peakWindow  int64 // largest window ever reached
	lowCoverage int64 // reads forced Random by the coverage gate (#221)
}

// covSpan is one read's byte range [off, end).
type covSpan struct{ off, end int64 }

// Halvings reports how many times a seek halved the window.
func (p *Prefetcher) Halvings() int64 { return p.halvings }

// Resets reports how many times the detector collapsed to Random.
func (p *Prefetcher) Resets() int64 { return p.resetRandom }

// PeakWindow reports the largest readahead window this handle ever reached.
func (p *Prefetcher) PeakWindow() int64 { return p.peakWindow }

// New returns a Prefetcher with the given max readahead window in blocks
// (values < 2 are raised to 2). The byte-gap gate is off by default (seqGapMax =
// MaxInt64); call SetGapMax to enable it.
func New(maxReadahead int64) *Prefetcher {
	if maxReadahead < initialWindow {
		maxReadahead = initialWindow
	}
	return &Prefetcher{maxReadahead: maxReadahead, state: Cold, seqGapMax: math.MaxInt64}
}

// SetCoverage enables the #221 coverage gate: over a trailing window of `window`
// reads, Sequential requires that the union of the reads' byte ranges covers at
// least `minRatio` of the span they touch. window<=1 or minRatio<=0 disables it.
// The characterization (#221) put streams at coverage >= 0.89 (W=16) and every
// scattered walk (netcdf4 0.002, GRIB 0.025, COG 0.069) at <= 0.07, so a wide
// window is required — a small one lets field-internal reads tile locally — and
// 0.5 sits in the middle of that order-of-magnitude margin.
func (p *Prefetcher) SetCoverage(window int, minRatio float64) {
	if window <= 1 || minRatio <= 0 {
		window, minRatio = 0, 0
	}
	p.covWindow = window
	p.covMin = minRatio
	p.covSpans = nil
}

// LowCoverage reports how many reads the coverage gate forced Random (#221).
func (p *Prefetcher) LowCoverage() int64 { return p.lowCoverage }

// recordRead pushes a read's byte range into the trailing ring and returns the
// coverage ratio (union of ranges / total span) over the window. Returns 1.0
// when the gate is disabled or fewer than 3 reads have been seen (too little to
// judge; the initial ramp is left to the normal state machine).
func (p *Prefetcher) recordRead(off, length int64) float64 {
	if p.covWindow <= 1 || length <= 0 {
		return 1.0
	}
	p.covSpans = append(p.covSpans, covSpan{off, off + length})
	if len(p.covSpans) > p.covWindow {
		p.covSpans = p.covSpans[len(p.covSpans)-p.covWindow:]
	}
	if len(p.covSpans) < 3 {
		return 1.0
	}
	lo, hi := p.covSpans[0].off, p.covSpans[0].end
	for _, s := range p.covSpans[1:] {
		if s.off < lo {
			lo = s.off
		}
		if s.end > hi {
			hi = s.end
		}
	}
	span := hi - lo
	if span <= 0 {
		return 1.0
	}
	// Union of the (unsorted, possibly overlapping) ranges.
	sp := append([]covSpan(nil), p.covSpans...)
	sort.Slice(sp, func(i, j int) bool { return sp[i].off < sp[j].off })
	var union int64
	cs, ce := sp[0].off, sp[0].end
	for _, s := range sp[1:] {
		if s.off > ce {
			union += ce - cs
			cs, ce = s.off, s.end
		} else if s.end > ce {
			ce = s.end
		}
	}
	union += ce - cs
	return float64(union) / float64(span)
}

// SetGapMax sets the largest byte gap between consecutive reads that still counts
// as sequential progress (#210/M16 1b). The FUSE layer sets it to the block size:
// a read landing more than one block past the previous is a seek, not a stream,
// however in-band it looks in block space. n<=0 disables the gate (MaxInt64).
func (p *Prefetcher) SetGapMax(n int64) {
	if n <= 0 {
		n = math.MaxInt64
	}
	p.seqGapMax = n
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// State returns the current detected pattern.
func (p *Prefetcher) State() State { return p.state }

// covReads is the minimum reads before coverage can judge tiling (#229). Below
// it the trailing window is too short to separate a stream from a walk, so the
// handle is treated as not-yet-established and served precise.
const covReads = 3

// coverageOK reports whether the coverage signal confirms tiling: the gate is on,
// enough reads have accumulated to judge, and the trailing coverage clears the
// threshold. cov is the ratio just returned by recordRead.
func (p *Prefetcher) coverageOK(cov float64) bool {
	if p.covMin <= 0 {
		return true // gate disabled: behaviour-preserving
	}
	return len(p.covSpans) >= covReads && cov >= p.covMin
}

// Established reports whether the access pattern is known to tile — the single
// gate the FUSE layer's open-time parts-fetch (#69/#220) and initial ramp (#228)
// consult before committing any broad fetch (#229). False until the coverage
// signal confirms tiling; false again if it later collapses.
func (p *Prefetcher) Established() bool { return p.established }

// SetMax updates the readahead window cap (in blocks). The FUSE layer lowers it
// when many handles are open so their windows share the prefetch budget (#55).
// The current window is clamped down to the new cap; the floor is initialWindow.
func (p *Prefetcher) SetMax(n int64) {
	if n < initialWindow {
		n = initialWindow
	}
	p.maxReadahead = n
	if p.window > n {
		p.window = n
	}
}

// Open sets up the handle but dispatches NO prefetch (#229): the initial ramp is
// a broad fetch, and the policy commits none until the access pattern is known to
// tile. The ramp begins from the first read that establishes coverage (read ~2),
// via Observe. With the coverage gate disabled it still dispatches the initial
// window, preserving the pre-#229 open-time ramp.
func (p *Prefetcher) Open() []int64 {
	p.haveLast = true
	p.lastBlock = -1 // so the first read at block 0 registers as sequential (d=1)
	p.state = Cold
	p.established = false
	if p.covMin > 0 {
		p.window = 0
		p.cursor = -1
		p.frontier = 0
		return nil
	}
	p.window = initialWindow
	p.cursor = -1
	p.frontier = initialWindow
	return blockRange(0, initialWindow)
}

// Observe records a demand read at blockIdx, byteGap bytes from the end of the
// previous read on this handle, and returns the block indices to dispatch now
// (the frontier advance), empty when the frontier already leads far enough.
//
// byteGap distinguishes a stream from a scattered walk that the block-space
// in-band test alone cannot (#210/M16 1b): a metadata walk reads kilobytes at
// offsets megabytes apart, so its reads land in adjacent 8 MiB blocks (d==1) or
// inside the grown reorder band and were misread as sequential. A read whose
// |gap| exceeds seqGapMax is not sequential progress however in-band it looks —
// it falls through to the seek path. Contiguous streams (gap≈0) and kernel
// reorder (gap ≪ block) are unaffected; seqGapMax defaults to "off" (MaxInt64)
// so a caller that does not set it keeps the pre-1b behavior.
func (p *Prefetcher) Observe(blockIdx, off, length, byteGap int64) []int64 {
	cov := p.recordRead(off, length)
	if !p.haveLast {
		p.haveLast = true
		p.lastBlock = blockIdx
		p.cursor = blockIdx
		return nil
	}
	d := blockIdx - p.lastBlock
	p.lastBlock = blockIdx

	// Re-read of the same block: no state change, no dispatch.
	if d == 0 {
		return nil
	}

	// Strided detection (before the seek rule): a constant non-unit delta seen
	// twice. Not evaluated once a sequential stream is established, so an
	// in-band reorder whose deltas happen to repeat is not misread as a stride.
	if d != 1 && d == p.lastDelta && p.state != Sequential {
		p.state = Strided
		p.pending = false
		p.lastDelta = d
		p.cursor = blockIdx
		pred := blockIdx + d
		if pred >= p.frontier {
			p.frontier = pred + 1
			return []int64{pred}
		}
		return nil
	}

	// Sequential progress: a unit step, or a read within the reorder band — but
	// only if the read is byte-contiguous. A large byte gap is a seek even when it
	// lands in an adjacent block or the grown band (#210/M16 1b: the scattered
	// metadata walk).
	if (d == 1 || p.inBand(blockIdx)) && absInt64(byteGap) <= p.seqGapMax {
		p.pending = false
		p.lastDelta = 1
		if blockIdx > p.cursor {
			p.cursor = blockIdx
		}
		// #229: recognize contiguous progress always, but commit a broad fetch
		// (the readahead ramp) only once coverage confirms the access tiles. Until
		// then the handle is provisional — the read is served precise and nothing
		// is dispatched — and a contiguous read whose window is still full of an
		// earlier scattered walk de-establishes rather than ramping.
		if !p.coverageOK(cov) {
			p.established = false
			if p.state == Sequential {
				p.state = Cold
			}
			p.window = 0
			p.frontier = p.cursor + 1
			return nil
		}
		if p.state == Sequential {
			p.window = min(p.window*2, p.maxReadahead)
		} else {
			// #229: on establishment jump straight to the full window rather than
			// re-ramping from 2. A genuine stream/copy pays only the first ~2 precise
			// reads; without this it also pays a geometric re-ramp (many small GETs,
			// an underfed NIC on the cold read — the #56 concern), which regressed
			// sequential copies. A handle that establishes then immediately seeks (a
			// per-field walk) reverts on the next landing, so the full window is
			// dispatched at most once per established run.
			p.state = Sequential
			if p.covMin > 0 {
				p.window = p.maxReadahead
			} else {
				p.window = initialWindow // gate off: pre-#229 geometric ramp
			}
		}
		p.established = true
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		if p.frontier < p.cursor+1 {
			p.frontier = p.cursor + 1
		}
		return p.advance(p.cursor + 1 + p.window)
	}

	// Out of band: a seek.
	p.lastDelta = d
	// Coverage gate (#221): a seek landing whose trailing coverage is low is a
	// scattered walk (metadata traversal, GRIB .idx field sweep, COG overview),
	// not a stream taking one jump. Do NOT optimistically re-anchor Sequential and
	// prefetch two blocks at the landing — that re-anchor, fired at every landing,
	// refilled whole sub-64-MiB objects (#220). Go Random and dispatch nothing; a
	// walk that later tiles re-establishes Sequential via the contiguous branch
	// above once coverage recovers. Streams never trip this (coverage ~1, and they
	// rarely reach the seek path at all). Disabled by default (covMin<=0).
	if p.covMin > 0 && cov < p.covMin {
		if p.state != Random {
			p.resetRandom++
		}
		p.lowCoverage++
		p.state = Random
		p.established = false // #229: a scattered landing de-establishes the handle
		p.pending = false
		p.window = 0
		p.cursor = blockIdx
		p.frontier = blockIdx
		return nil
	}
	if p.pending {
		// A second unexplained jump with no sequential progress between: stop
		// prefetching until a sequential run resumes.
		p.state = Random
		p.established = false
		p.pending = false
		p.window = 0
		p.cursor = blockIdx
		p.frontier = blockIdx
		p.resetRandom++
		return nil
	}
	if p.state == Sequential || p.state == Strided {
		// One jump out of an established pattern: halve the window (floor 2) —
		// not a full reset to zero, which is the #49 lone-survivor re-ramp — and
		// re-anchor at b with the initial 2-block dispatch.
		p.window = max(initialWindow, p.window/2)
		p.halvings++
		if p.window > p.peakWindow {
			p.peakWindow = p.window
		}
		p.state = Sequential
		p.pending = true
		p.cursor = blockIdx
		p.frontier = blockIdx
		return p.advance(blockIdx + initialWindow)
	}
	// From Cold/Random with no pending jump: tentative — wait to see if this is
	// a stride or a seek before committing.
	p.pending = true
	return nil
}

// inBand reports whether b is within the reorder band around the current
// cursor/frontier: [cursor-window, frontier+window], with at least the initial
// window of slack so re-entry from a zero window still recognises progress.
func (p *Prefetcher) inBand(b int64) bool {
	w := p.window
	if w < initialWindow {
		w = initialWindow
	}
	return b >= p.cursor-w && b <= p.frontier+w
}

// advance dispatches [frontier, target) and moves the frontier to target.
func (p *Prefetcher) advance(target int64) []int64 {
	if target <= p.frontier {
		return nil
	}
	lo := p.frontier
	p.frontier = target
	return blockRange(lo, target-lo)
}

// blockRange returns [start, start+count).
func blockRange(start, count int64) []int64 {
	if count <= 0 {
		return nil
	}
	out := make([]int64, count)
	for i := int64(0); i < count; i++ {
		out[i] = start + i
	}
	return out
}
